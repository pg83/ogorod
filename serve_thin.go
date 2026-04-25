package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
)

// serveThinMain implements `ogorod serve-thin`. Unlike `ogorod serve`
// (the "fat" mode), the cluster sync logic is moved out of the HTTP
// handler and into per-request shell wrappers around git-upload-pack
// and git-receive-pack. The handler itself does the bare minimum:
//
//  1. parse repo from URL,
//  2. ensure the bare-repo skeleton exists,
//  3. on push, hold the etcd lease lock,
//  4. exec git-http-backend with PATH prepended by a bin-dir that
//     contains our wrappers,
//  5. stream the CGI response.
//
// The wrappers (installed once at startup) re-exec the binary as
// `ogorod wrap upload-pack` / `ogorod wrap receive-pack`, which run
// SyncRepo() and then syscall.Exec the real git binary. Net effect:
// fetch and push pay the sync cost lazily, only when a real protocol
// op happens, instead of the fat-mode "sync once per HTTP request".
func serveThinMain(args []string) {
	listen := ":8038"
	cacheDir := "/var/cache/ogorod-thin"
	binDir := "/var/run/ogorod_thin/bin"

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--listen":
			i++

			if i >= len(args) {
				ThrowFmt("--listen needs an argument")
			}

			listen = args[i]

		case "--cache-dir":
			i++

			if i >= len(args) {
				ThrowFmt("--cache-dir needs an argument")
			}

			cacheDir = args[i]

		case "--bin-dir":
			i++

			if i >= len(args) {
				ThrowFmt("--bin-dir needs an argument")
			}

			binDir = args[i]

		default:
			ThrowFmt("unknown flag: %q", args[i])
		}
	}

	Throw(os.MkdirAll(cacheDir, 0o755))
	Throw(os.MkdirAll(binDir, 0o755))

	installPathWrappers(binDir)

	env := loadEnv()

	srv := &thinServer{
		env:      env,
		cacheDir: cacheDir,
		binDir:   binDir,
		repoMux:  make(map[string]*sync.Mutex),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handle)

	fmt.Fprintf(os.Stderr, "ogorod serve-thin: listening on %s, cache %s, bin %s\n",
		listen, cacheDir, binDir)

	Throw(http.ListenAndServe(listen, mux))
}

type thinServer struct {
	env      Env
	cacheDir string
	binDir   string

	mu      sync.Mutex
	repoMux map[string]*sync.Mutex
}

func (s *thinServer) repoLock(repo string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()

	mu, ok := s.repoMux[repo]

	if !ok {
		mu = &sync.Mutex{}
		s.repoMux[repo] = mu
	}

	return mu
}

func (s *thinServer) handle(w http.ResponseWriter, r *http.Request) {
	exc := Try(func() {
		s.handleInner(w, r)
	})

	exc.Catch(func(e *Exception) {
		fmt.Fprintf(os.Stderr, "ogorod serve-thin: %s %s: %s\n", r.Method, r.URL.Path, e.Error())
		http.Error(w, e.Error(), http.StatusInternalServerError)
	})
}

func (s *thinServer) handleInner(w http.ResponseWriter, r *http.Request) {
	repo, subpath, ok := parseRepoURL(r.URL.Path)

	if !ok {
		http.NotFound(w, r)

		return
	}

	mu := s.repoLock(repo)
	mu.Lock()
	defer mu.Unlock()

	cacheDir := filepath.Join(s.cacheDir, repo+".git")

	// Bare-repo skeleton must exist before git-http-backend touches
	// it. The wrapper does the actual sync from S3+etcd before each
	// upload-pack/receive-pack invocation.
	ensureBareRepo(cacheDir)

	isPush := subpath == "/git-receive-pack" ||
		(subpath == "/info/refs" && r.URL.Query().Get("service") == "git-receive-pack")

	if isPush {
		ec := newEtcdClient(s.env, repo)
		defer ec.Close()

		lock := ec.AcquireLock(r.Context())
		defer lock.Unlock(r.Context())
	}

	dispatchToBackendThin(w, r, cacheDir, repo, subpath, s.env, s.binDir)
}

// dispatchToBackendThin is dispatchToBackend with PATH manipulated
// so git-http-backend finds our wrappers first. Everything else is
// identical: stdin/stdout/stderr piping, CGI parsing.
func dispatchToBackendThin(w http.ResponseWriter, r *http.Request, cacheDir, repo, subpath string, env Env, binDir string) {
	cmd := exec.CommandContext(r.Context(), "git", "http-backend")

	cmd.Env = backendEnvThin(r, cacheDir, repo, subpath, env, binDir)
	cmd.Stdin = r.Body

	stdout := Throw2(cmd.StdoutPipe())
	stderr := Throw2(cmd.StderrPipe())

	Throw(cmd.Start())

	go func() {
		Throw2(io.Copy(os.Stderr, stderr))
	}()

	parseAndStreamCGIResponse(stdout, w)

	Throw(cmd.Wait())
}

// backendEnvThin is backendEnv with PATH prepended by binDir so
// git-http-backend's execvp("git", …) finds our shadow `git`
// first. The shadow re-exec's `ogorod wrap`, which (after sync)
// strips binDir from PATH and syscall.Exec's the captured real
// git — see wrap.go scrubEnv.
//
// OGOROD_REAL_GIT and OGOROD_THIN_BIN_DIR are propagated so the
// wrap subcommand running as a child knows where to go and what
// to scrub.
func backendEnvThin(r *http.Request, cacheDir, repo, subpath string, env Env, binDir string) []string {
	out := backendEnv(r, cacheDir, repo, subpath, env)

	out = setEnv(out, "PATH", binDir+":"+os.Getenv("PATH"))
	out = setEnv(out, "OGOROD_REAL_GIT", os.Getenv("OGOROD_REAL_GIT"))
	out = setEnv(out, "OGOROD_THIN_BIN_DIR", binDir)

	return out
}

func setEnv(out []string, key, value string) []string {
	for i, kv := range out {
		if k, _, _ := splitEnv(kv); k == key {
			out[i] = key + "=" + value

			return out
		}
	}

	return append(out, key+"="+value)
}

func splitEnv(kv string) (k, v string, ok bool) {
	for i := 0; i < len(kv); i++ {
		if kv[i] == '=' {
			return kv[:i], kv[i+1:], true
		}
	}

	return kv, "", false
}

// installPathWrappers writes a single shadow `git` script into
// binDir. git-http-backend runs `git upload-pack <dir>` and
// `git receive-pack <dir>` via execvp; PATH lookup finds our
// shadow first, which re-exec's us as `ogorod wrap <args>`.
//
// The wrap subcommand syncs the cache (only for upload-pack /
// receive-pack — passes other invocations through) and then
// syscall.Exec's the real git captured at startup.
//
// Why a `git` shim rather than per-helper (`git-upload-pack`)
// shims: in modern git, upload-pack and receive-pack are builtins
// of the main `git` binary — there are no external git-upload-pack
// / git-receive-pack processes to intercept. Wrapping `git` itself
// is the only point that catches both.
func installPathWrappers(binDir string) {
	binPath := Throw2(os.Executable())

	realGit := Throw2(exec.LookPath("git"))

	resolved, err := filepath.EvalSymlinks(realGit)

	if err == nil && resolved != "" {
		realGit = resolved
	}

	fmt.Fprintf(os.Stderr, "ogorod serve-thin: real git at %s; wrapping into %s\n", realGit, binDir)

	scriptPath := filepath.Join(binDir, "git")

	os.Remove(scriptPath)

	wrapper := "#!/bin/sh\nexec " + binPath + " wrap \"$@\"\n"
	Throw(os.WriteFile(scriptPath, []byte(wrapper), 0o755))

	// Stash the resolved real-git path in the env so wrap.go can
	// syscall.Exec it without doing its own PATH lookup (PATH now
	// has our shim first). Cleared on each startup.
	Throw(os.Setenv("OGOROD_REAL_GIT", realGit))
	Throw(os.Setenv("OGOROD_THIN_BIN_DIR", binDir))
}

