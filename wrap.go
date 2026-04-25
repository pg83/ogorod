package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// wrapMain implements the shadow `git` we install in serve-thin's
// bin-dir. git-http-backend invokes `git upload-pack <dir>` /
// `git receive-pack <dir>` via execvp("git", …); PATH lookup finds
// our wrapper first, the wrapper re-exec's us as `ogorod wrap …`,
// and we get a chance to:
//
//   - sync the local cache against S3+etcd before the protocol
//     command runs (only for upload-pack / receive-pack — every
//     other git invocation passes through unchanged);
//   - install our pre-receive hook in the cache repo;
//   - syscall.Exec the real git binary with the same argv, with
//     bin-dir stripped from PATH so the in-process upload-pack /
//     receive-pack builtins don't re-find our wrapper if they
//     spawn further child gits.
//
// The real git path is captured by serve-thin at startup (before
// it shadows PATH) and passed via OGOROD_REAL_GIT — we can't look
// it up here, since LookPath now hits our own wrapper.
//
// Why argv[0]="git" interception rather than the per-helper
// (git-upload-pack) layer: in modern git (2.x), upload-pack and
// receive-pack are builtins of the main git binary, so there is
// no separate git-upload-pack process to wrap. The HTTP backend
// runs them in-process via cmd_upload_pack() / cmd_receive_pack()
// once the main `git` binary is started. Wrapping `git` itself is
// therefore the only interception point that catches both.
func wrapMain(args []string) {
	if len(args) < 1 {
		ThrowFmt("usage: ogorod wrap <git-args...>")
	}

	sub := args[0]

	// Only the two protocol commands need cluster-sync.
	// Everything else (config, rev-parse, …) is a fall-through.
	if sub == "upload-pack" || sub == "receive-pack" {
		gitDir := findRepoDir(args[1:])

		if gitDir != "" {
			doSync(sub, gitDir)
		}
	}

	realGit := os.Getenv("OGOROD_REAL_GIT")

	if realGit == "" {
		ThrowFmt("OGOROD_REAL_GIT not set; serve-thin should propagate it")
	}

	envOut := scrubEnv(os.Environ())

	Throw(syscall.Exec(realGit, append([]string{"git"}, args...), envOut))
}

func doSync(sub, gitDir string) {
	repo := strings.TrimSuffix(filepath.Base(gitDir), ".git")

	fmt.Fprintf(os.Stderr, "ogorod wrap %s: dir=%s repo=%s\n", sub, gitDir, repo)

	env := loadEnv()

	ec := newEtcdClient(env, repo)
	defer ec.Close()

	s3 := newS3Client(env, repo)

	SyncRepo(context.Background(), ec, s3, gitDir)

	installHook(gitDir)
}

// findRepoDir returns the first non-flag argument — the bare-repo
// path that git-http-backend hands to upload-pack / receive-pack.
func findRepoDir(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}

	return ""
}

// scrubEnv removes the bin-dir entry from PATH and drops any
// GIT_EXEC_PATH override so the real git falls back to its
// compiled-in defaults — otherwise child processes spawned from
// in-process upload-pack / receive-pack could re-enter our wrapper.
func scrubEnv(in []string) []string {
	binDir := ""

	if exe, err := os.Executable(); err == nil {
		binDir = filepath.Dir(exe)

		// os.Executable() returns the ogorod binary's directory,
		// not the wrapper bin-dir. Use OGOROD_THIN_BIN_DIR which
		// serve-thin sets explicitly so we can strip it.
	}

	if v := os.Getenv("OGOROD_THIN_BIN_DIR"); v != "" {
		binDir = v
	}

	out := make([]string, 0, len(in))

	for _, kv := range in {
		k, v, _ := strings.Cut(kv, "=")

		switch k {
		case "GIT_EXEC_PATH":
			continue

		case "PATH":
			if binDir != "" {
				v = stripPathEntry(v, binDir)
			}

			out = append(out, "PATH="+v)

		default:
			out = append(out, kv)
		}
	}

	return out
}

// stripPathEntry removes one literal entry from a colon-separated
// PATH string. No-op if the entry isn't present.
func stripPathEntry(path, entry string) string {
	parts := strings.Split(path, ":")

	out := parts[:0]

	for _, p := range parts {
		if p == entry {
			continue
		}

		out = append(out, p)
	}

	return strings.Join(out, ":")
}

// Defensive: if the wrapper is invoked with no real-git in env
// (someone called us with a different setup), at least try
// LookPath. This branch is unreachable from serve-thin's normal
// path; kept so the code is still self-describing.
var _ = exec.LookPath
