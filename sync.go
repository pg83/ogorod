package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// SyncRepo brings the local cache directory in sync with the
// authoritative state in S3 (packs) and etcd (refs + HEAD).
//
// Two-step: the version key in etcd is bumped on every successful
// push; if the locally-recorded version equals the remote, no work
// to do. Otherwise we list S3 packs, download any missing ones,
// regenerate packed-refs from the etcd ref map, write HEAD, and
// record the new version locally.
//
// Idempotent. Safe to call from multiple request handlers in the
// same server (a per-repo mutex in serve.go serializes them so
// the disk state is coherent).
func SyncRepo(ctx context.Context, ec *EtcdClient, s3 *S3Client, cacheDir string) {
	ensureBareRepo(cacheDir)

	remoteVersion := ec.GetVersion(ctx)
	localVersion := readLocalVersion(cacheDir)

	if localVersion == remoteVersion {
		// Even at version 0 (fresh repo) we still need to write
		// out an empty packed-refs / HEAD on first call so git
		// can serve info/refs sanely. ensureBareRepo handled the
		// minimum; for an existing-but-empty repo we're done.
		return
	}

	syncPacks(ctx, s3, cacheDir)

	refs := ec.ListRefs(ctx)
	writePackedRefs(cacheDir, refs)

	if head, ok := refs["HEAD"]; ok {
		writeHead(cacheDir, head)
	}

	writeLocalVersion(cacheDir, remoteVersion)
}

// ensureBareRepo creates the minimal layout git expects for a bare
// repo if cacheDir doesn't yet exist. Idempotent.
//
// Writes HEAD + packed-refs unconditionally on first creation so a
// fresh repo (etcd version=0, never pushed) still answers info/refs
// instead of 404'ing when git-http-backend tries to read its layout.
// SyncRepo will overwrite HEAD/packed-refs once etcd has real state.
func ensureBareRepo(cacheDir string) {
	for _, sub := range []string{"objects/pack", "objects/info", "refs/heads", "refs/tags", "info", "hooks"} {
		Throw(os.MkdirAll(filepath.Join(cacheDir, sub), 0o755))
	}

	cfgPath := filepath.Join(cacheDir, "config")

	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		// http.receivepack: enable push over smart HTTP.
		// transfer.unpackLimit=0 + receive.unpackLimit=0: always
		// keep incoming data as a packfile (never explode into
		// loose). Our hook only knows how to ship packs to S3.
		Throw(os.WriteFile(cfgPath, []byte(
			"[core]\n"+
				"\trepositoryformatversion = 0\n"+
				"\tfilemode = true\n"+
				"\tbare = true\n"+
				"[http]\n"+
				"\treceivepack = true\n"+
				"[transfer]\n"+
				"\tunpackLimit = 0\n"+
				"[receive]\n"+
				"\tunpackLimit = 0\n",
		), 0o644))
	}

	headPath := filepath.Join(cacheDir, "HEAD")

	if _, err := os.Stat(headPath); os.IsNotExist(err) {
		// Placeholder HEAD until etcd has a real one. The hook's
		// PutHEADIfMissing on first push will pick whatever branch
		// the user actually creates; sync will then overwrite this.
		Throw(os.WriteFile(headPath, []byte("ref: refs/heads/master\n"), 0o644))
	}

	refsPath := filepath.Join(cacheDir, "packed-refs")

	if _, err := os.Stat(refsPath); os.IsNotExist(err) {
		Throw(os.WriteFile(refsPath, []byte("# pack-refs with: peeled fully-peeled sorted\n"), 0o644))
	}
}

// syncPacks downloads any S3 pack/idx pair not already present in
// the local cache. Reuses pack.go's loadPacksIntoLocal.
func syncPacks(ctx context.Context, s3 *S3Client, cacheDir string) {
	loadPacksIntoLocal(ctx, s3, cacheDir)
}

// writePackedRefs flushes the etcd ref map to <cacheDir>/packed-refs
// in git's standard format. HEAD is excluded — handled separately.
//
// Header declares "sorted" so git's packed-refs reader uses bisect
// for ref lookups (and asserts on any out-of-order entry — which
// is the BUG-then-corruption-abort path you don't want git in).
// Map iteration in Go is randomised, so we explicitly sort by ref
// name before emitting.
func writePackedRefs(cacheDir string, refs map[string]string) {
	names := make([]string, 0, len(refs))

	for name := range refs {
		if name == "HEAD" {
			continue
		}

		names = append(names, name)
	}

	sort.Strings(names)

	var b strings.Builder

	b.WriteString("# pack-refs with: peeled fully-peeled sorted\n")

	for _, name := range names {
		fmt.Fprintf(&b, "%s %s\n", refs[name], name)
	}

	Throw(os.WriteFile(filepath.Join(cacheDir, "packed-refs"), []byte(b.String()), 0o644))
}

// writeHead writes the HEAD file. Etcd stores either "ref: refs/..."
// (the symbolic-ref form, which is the normal case) or a raw sha
// (rare, detached HEAD). Either way git accepts the literal content.
func writeHead(cacheDir, value string) {
	Throw(os.WriteFile(filepath.Join(cacheDir, "HEAD"), []byte(value+"\n"), 0o644))
}

func readLocalVersion(cacheDir string) int64 {
	bytes, err := os.ReadFile(filepath.Join(cacheDir, ".ogorod-version"))

	if err != nil {
		return 0
	}

	v, err := strconv.ParseInt(strings.TrimSpace(string(bytes)), 10, 64)

	if err != nil {
		return 0
	}

	return v
}

func writeLocalVersion(cacheDir string, v int64) {
	Throw(os.WriteFile(
		filepath.Join(cacheDir, ".ogorod-version"),
		[]byte(strconv.FormatInt(v, 10)),
		0o644,
	))
}
