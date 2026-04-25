package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// wrapMain implements:
//
//	ogorod wrap upload-pack  [args...]
//	ogorod wrap receive-pack [args...]
//
// Used by `ogorod serve-thin` as a transparent shim around the
// native git-{upload,receive}-pack binaries:
//
//  1. derive the repo name from the bare-repo dir argv contains;
//  2. SyncRepo the local cache against S3+etcd;
//  3. syscall.Exec the real `git <subcommand>` with the same args
//     so stdin/stdout/stderr stay wired to git-http-backend.
//
// Step 3 uses syscall.Exec rather than os/exec.Run so this process
// disappears entirely, leaving the real binary as the direct child
// of git-http-backend — same exit semantics as if the wrapper had
// never been there.
//
// `git <subcommand>` resolves the helper through GIT_EXEC_PATH,
// which does not include our wrapper bin-dir, so there's no risk
// of looping back into ourselves.
func wrapMain(args []string) {
	if len(args) < 1 {
		ThrowFmt("usage: ogorod wrap {upload-pack|receive-pack} [args...]")
	}

	sub := args[0]
	rest := args[1:]

	if sub != "upload-pack" && sub != "receive-pack" {
		ThrowFmt("ogorod wrap: unknown subcommand %q", sub)
	}

	gitDir := findRepoDir(rest)

	if gitDir == "" {
		ThrowFmt("ogorod wrap %s: no repo dir in args: %v", sub, rest)
	}

	repo := strings.TrimSuffix(filepath.Base(gitDir), ".git")

	env := loadEnv()
	ec := newEtcdClient(env, repo)
	defer ec.Close()
	s3 := newS3Client(env, repo)

	SyncRepo(context.Background(), ec, s3, gitDir)

	// pre-receive hook: ensure it's installed (idempotent). Runs in
	// thin mode too — same code path uploads packs to S3 + CAS refs.
	installHook(gitDir)

	gitPath := Throw2(exec.LookPath("git"))

	realArgs := append([]string{"git", sub}, rest...)

	// Drop GIT_EXEC_PATH so the real git resolves through its
	// compiled-in exec-path, not the wrapper-shadow dir we came
	// from — otherwise `git upload-pack` re-finds our wrapper and
	// loops forever.
	envOut := make([]string, 0, len(os.Environ()))

	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_EXEC_PATH=") {
			continue
		}

		envOut = append(envOut, kv)
	}

	Throw(syscall.Exec(gitPath, realArgs, envOut))
}

// findRepoDir returns the first non-flag argument, which is the
// bare-repo path git-http-backend passes to upload-pack/receive-pack.
func findRepoDir(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}

	return ""
}
