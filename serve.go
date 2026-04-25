package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// serveMain implements `ogorod serve [--listen ADDR] [--cache-dir DIR]`.
//
// Vanilla git clients clone/push via:
//
//	git clone http://<host>:<port>/<repo>.git
//
// One server per lab host; all 3 share state via S3 (packs) +
// etcd (refs, locks, version). HA model: any server handles any
// request. Pushes serialize on an etcd mutex; fetches don't.
func serveMain(args []string) {
	listen := ":8035"
	cacheDir := "/var/cache/ogorod"

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

		default:
			ThrowFmt("unknown flag: %q", args[i])
		}
	}

	Throw(os.MkdirAll(cacheDir, 0o755))

	env := loadEnv()

	srv := &server{
		env:      env,
		cacheDir: cacheDir,
		repoMux:  make(map[string]*sync.Mutex),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", srv.handle)

	fmt.Fprintf(os.Stderr, "ogorod serve: listening on %s, cache %s\n", listen, cacheDir)

	Throw(http.ListenAndServe(listen, mux))
}

type server struct {
	env      Env
	cacheDir string

	mu      sync.Mutex
	repoMux map[string]*sync.Mutex
}

// repoLock returns a per-repo mutex used to serialize cache-state
// touches inside this process. Cross-process / cross-host coherence
// is the etcd RepoLock's job, not this one.
func (s *server) repoLock(repo string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()

	mu, ok := s.repoMux[repo]

	if !ok {
		mu = &sync.Mutex{}
		s.repoMux[repo] = mu
	}

	return mu
}

func (s *server) handle(w http.ResponseWriter, r *http.Request) {
	exc := Try(func() {
		s.handleInner(w, r)
	})

	exc.Catch(func(e *Exception) {
		fmt.Fprintf(os.Stderr, "ogorod serve: %s %s: %s\n", r.Method, r.URL.Path, e.Error())
		http.Error(w, e.Error(), http.StatusInternalServerError)
	})
}

func (s *server) handleInner(w http.ResponseWriter, r *http.Request) {
	repo, subpath, ok := parseRepoURL(r.URL.Path)

	if !ok {
		http.NotFound(w, r)

		return
	}

	mu := s.repoLock(repo)
	mu.Lock()
	defer mu.Unlock()

	cacheDir := filepath.Join(s.cacheDir, repo+".git")

	ec := newEtcdClient(s.env, repo)
	defer ec.Close()
	s3 := newS3Client(s.env, repo)

	isPush := subpath == "/git-receive-pack" ||
		(subpath == "/info/refs" && r.URL.Query().Get("service") == "git-receive-pack")

	// Etcd lock spans the whole push: info/refs (advertisement)
	// and the subsequent receive-pack POST. Two-phase locking
	// keeps another server's concurrent push from advertising a
	// later state mid-push and confusing the client.
	if isPush {
		lock := ec.AcquireLock(r.Context())
		defer lock.Unlock(r.Context())
	}

	SyncRepo(r.Context(), ec, s3, cacheDir)

	installHook(cacheDir)

	dispatchToBackend(w, r, cacheDir, repo, subpath, s.env)
}

// parseRepoURL extracts the repo name and the post-".git" subpath.
// Accepts the three URL forms git uses for smart HTTP:
//
//	/<repo>.git/info/refs
//	/<repo>.git/git-upload-pack
//	/<repo>.git/git-receive-pack
//
// Repo may contain slashes for hierarchical naming (user/repo).
func parseRepoURL(path string) (repo, subpath string, ok bool) {
	if !strings.HasPrefix(path, "/") {
		return "", "", false
	}

	rest := path[1:]
	idx := strings.Index(rest, ".git/")

	if idx < 0 {
		return "", "", false
	}

	repo = rest[:idx]
	subpath = "/" + rest[idx+len(".git/"):]

	if repo == "" {
		return "", "", false
	}

	return repo, subpath, true
}

// installHook writes a tiny shell wrapper at <cacheDir>/hooks/pre-receive
// that re-execs us in `ogorod hook` mode. Idempotent — only writes
// when the existing content differs (so we don't churn mtime on
// every request).
func installHook(cacheDir string) {
	binPath := Throw2(os.Executable())

	wrapper := "#!/bin/sh\nexec " + binPath + " hook\n"

	hookPath := filepath.Join(cacheDir, "hooks", "pre-receive")

	if existing, err := os.ReadFile(hookPath); err == nil && string(existing) == wrapper {
		return
	}

	Throw(os.WriteFile(hookPath, []byte(wrapper), 0o755))
}

// dispatchToBackend execs git-http-backend (CGI) with the request
// body on stdin and translates its CGI-format stdout into the HTTP
// response. Stderr from the backend is forwarded to our stderr for
// debugging.
//
// CGI env: per RFC 3875 + git-http-backend's own variables. We
// also leak our own OGOROD_* + GIT_DIR/GIT_PROJECT_ROOT via the
// child process so the pre-receive hook can talk to S3+etcd.
func dispatchToBackend(w http.ResponseWriter, r *http.Request, cacheDir, repo, subpath string, env Env) {
	cmd := exec.CommandContext(r.Context(), "git", "http-backend")

	cmd.Env = backendEnv(r, cacheDir, repo, subpath, env)
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

// backendEnv assembles the CGI env for git-http-backend. The repo
// path is rooted at <cacheDir>/.. (parent of the per-repo dir) so
// PATH_INFO=/<repo>.git/... resolves correctly.
func backendEnv(r *http.Request, cacheDir, repo, subpath string, env Env) []string {
	out := []string{
		"PATH=" + os.Getenv("PATH"),
		"GIT_PROJECT_ROOT=" + filepath.Dir(cacheDir),
		"GIT_HTTP_EXPORT_ALL=1",
		"PATH_INFO=/" + repo + ".git" + subpath,
		"REQUEST_METHOD=" + r.Method,
		"QUERY_STRING=" + r.URL.RawQuery,
		"CONTENT_TYPE=" + r.Header.Get("Content-Type"),
		"REMOTE_ADDR=" + r.RemoteAddr,
		"REMOTE_USER=",

		// Propagate so the pre-receive hook can talk to backends.
		"OGOROD_S3_ENDPOINT=" + env.S3Endpoint,
		"OGOROD_S3_ACCESS_KEY=" + env.S3AccessKey,
		"OGOROD_S3_SECRET_KEY=" + env.S3SecretKey,
		"OGOROD_S3_BUCKET=" + env.S3Bucket,
		"OGOROD_ETCD_ENDPOINTS=" + strings.Join(env.EtcdEndpoints, ","),
		"OGOROD_REPO=" + repo,
	}

	if cl := r.Header.Get("Content-Length"); cl != "" {
		out = append(out, "CONTENT_LENGTH="+cl)
	}

	if ce := r.Header.Get("Content-Encoding"); ce != "" {
		out = append(out, "HTTP_CONTENT_ENCODING="+ce)
	}

	return out
}

// parseAndStreamCGIResponse reads CGI-format output:
//
//	Status: 200 OK\r\n            (optional, defaults to 200)
//	Content-Type: ...\r\n
//	<other headers>\r\n
//	\r\n
//	<body>
//
// translating into the http.ResponseWriter. Body is streamed.
func parseAndStreamCGIResponse(stdout io.Reader, w http.ResponseWriter) {
	br := bufio.NewReader(stdout)

	statusCode := http.StatusOK

	for {
		line, err := br.ReadString('\n')
		Throw(err)

		line = strings.TrimRight(line, "\r\n")

		if line == "" {
			break
		}

		k, v, ok := strings.Cut(line, ": ")

		if !ok {
			continue
		}

		if strings.EqualFold(k, "Status") {
			parts := strings.SplitN(v, " ", 2)

			n, perr := strconv.Atoi(parts[0])

			if perr == nil {
				statusCode = n
			}

			continue
		}

		w.Header().Set(k, v)
	}

	w.WriteHeader(statusCode)

	Throw2(io.Copy(w, br))
}
