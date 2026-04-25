package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// backendEnvThin is backendEnv with GIT_EXEC_PATH pointed at binDir
// so git-http-backend's `git_cmd`-mode launch of upload-pack /
// receive-pack picks up our wrappers. We also prepend binDir to PATH
// — defensive, in case some inner git path lookup falls back to it.
//
// The wrappers themselves syscall.Exec real `git <subcommand>`,
// which goes back through git's exec-path lookup; since the real
// `git` binary's exec-path is shadowed, we explicitly drop
// GIT_EXEC_PATH from the env we hand to syscall.Exec inside the
// wrap subcommand to break the loop. (See wrap.go.)
func backendEnvThin(r *http.Request, cacheDir, repo, subpath string, env Env, binDir string) []string {
	out := backendEnv(r, cacheDir, repo, subpath, env)

	out = setEnv(out, "GIT_EXEC_PATH", binDir)
	out = setEnv(out, "PATH", binDir+":"+os.Getenv("PATH"))

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

// installPathWrappers populates binDir with a "shadow" git exec-path:
// every helper from the real exec-path is symlinked in, then we
// overlay our wrappers for git-upload-pack and git-receive-pack.
//
// git-http-backend uses run_command(git_cmd=1) for upload-pack /
// receive-pack, which resolves through GIT_EXEC_PATH rather than
// PATH. Setting GIT_EXEC_PATH=binDir makes the backend pick up our
// wrappers; the symlinks make sure the rest of git's helpers
// (rev-parse, index-pack, …) still resolve.
func installPathWrappers(binDir string) {
	binPath := Throw2(os.Executable())

	realExec := strings.TrimSpace(string(Throw2(exec.Command("git", "--exec-path").Output())))

	if realExec == "" {
		ThrowFmt("git --exec-path returned empty")
	}

	fmt.Fprintf(os.Stderr, "ogorod serve-thin: shadowing git exec-path %s into %s\n", realExec, binDir)

	entries := Throw2(os.ReadDir(realExec))

	for _, e := range entries {
		name := e.Name()
		link := filepath.Join(binDir, name)

		// Stale symlink/file: replace.
		os.Remove(link)
		Throw(os.Symlink(filepath.Join(realExec, name), link))
	}

	fmt.Fprintf(os.Stderr, "ogorod serve-thin: linked %d helpers from %s\n", len(entries), realExec)

	// Overlay our wrappers — overwrite the symlinks we just made.
	for _, sub := range []string{"upload-pack", "receive-pack"} {
		path := filepath.Join(binDir, "git-"+sub)
		os.Remove(path)
		writeWrapper(binDir, "git-"+sub, binPath, sub)
	}
}

func writeWrapper(binDir, name, ogorodBin, sub string) {
	content := "#!/bin/sh\nexec " + ogorodBin + " wrap " + sub + " \"$@\"\n"

	path := filepath.Join(binDir, name)

	if existing, err := os.ReadFile(path); err == nil && string(existing) == content {
		return
	}

	Throw(os.WriteFile(path, []byte(content), 0o755))
}
