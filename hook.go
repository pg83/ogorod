package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// hookMain implements the pre-receive hook git invokes after the
// receive-pack process has written the incoming packfile to disk
// but before refs are updated. We:
//
//  1. Read ref-updates from stdin (one line per update:
//     "<old-sha> <new-sha> <ref-name>")
//  2. Identify packs newly-arrived in objects/pack/ by diffing
//     local vs S3 — anything local but not on S3 is from this push
//  3. Upload those packs (pack first, then idx) to S3
//  4. CAS the ref updates atomically in etcd
//  5. Bump the repo version so other servers re-sync
//
// Any failure → non-zero exit → git-receive-pack rejects the push.
//
// Env vars (set by serve.go's backendEnv):
//
//	OGOROD_REPO            — repo identifier
//	OGOROD_S3_*            — S3 connection
//	OGOROD_ETCD_ENDPOINTS  — etcd connection
//	GIT_DIR                — bare-repo path (set by receive-pack)
func hookMain(args []string) {
	repo := os.Getenv("OGOROD_REPO")

	if repo == "" {
		ThrowFmt("OGOROD_REPO not set; hook invoked outside ogorod serve?")
	}

	gitDir := os.Getenv("GIT_DIR")

	if gitDir == "" {
		gitDir = "."
	}

	gitDir = Throw2(filepath.Abs(gitDir))

	updates := readRefUpdates()

	if len(updates) == 0 {
		return
	}

	env := loadEnv()
	ec := newEtcdClient(env, repo)
	defer ec.Close()
	s3 := newS3Client(env, repo)
	ctx := context.Background()

	uploadNewPacks(ctx, s3, gitDir)

	if !casRefs(ctx, ec, updates) {
		ThrowFmt("ref CAS failed (concurrent update or non-fast-forward)")
	}

	maybeInitHEAD(ctx, ec, updates)

	ec.BumpVersion(ctx)
}

type hookUpdate struct {
	OldSha string
	NewSha string
	Ref    string
}

func readRefUpdates() []hookUpdate {
	var out []hookUpdate

	scanner := bufio.NewScanner(os.Stdin)

	for scanner.Scan() {
		line := scanner.Text()

		parts := strings.Fields(line)

		if len(parts) != 3 {
			ThrowFmt("malformed pre-receive line: %q", line)
		}

		out = append(out, hookUpdate{
			OldSha: parts[0],
			NewSha: parts[1],
			Ref:    parts[2],
		})
	}

	Throw(scanner.Err())

	return out
}

// uploadNewPacks scans <gitDir>/objects/pack for *.pack files that
// don't yet exist in S3, and uploads them (pack first, then idx).
//
// Diffing against S3 (rather than mtime) is robust: even if the
// hook runs minutes after receive-pack wrote its files, or if a
// previous failed hook left local-only packs around, the right
// thing happens — anything S3-missing gets shipped.
func uploadNewPacks(ctx context.Context, s3 *S3Client, gitDir string) {
	packDir := filepath.Join(gitDir, "objects", "pack")

	remote := s3.ListPacks(ctx)

	remoteSet := make(map[string]struct{}, len(remote))

	for _, n := range remote {
		remoteSet[n] = struct{}{}
	}

	entries := Throw2(os.ReadDir(packDir))

	for _, e := range entries {
		name := e.Name()

		if !strings.HasSuffix(name, ".pack") {
			continue
		}

		base := strings.TrimSuffix(name, ".pack")

		if _, ok := remoteSet[base]; ok {
			continue
		}

		packBytes := Throw2(os.ReadFile(filepath.Join(packDir, base+".pack")))
		idxBytes := Throw2(os.ReadFile(filepath.Join(packDir, base+".idx")))

		// Pack before idx: a fetcher that lists by .idx and finds
		// one must also find the matching .pack.
		s3.PutPack(ctx, base+".pack", packBytes)
		s3.PutPack(ctx, base+".idx", idxBytes)

		fmt.Fprintf(os.Stderr, "ogorod-hook: uploaded %s.pack (%d bytes), %s.idx (%d bytes)\n",
			base, len(packBytes), base, len(idxBytes))
	}
}

// casRefs maps git's "0000…0000" sentinel for create/delete to our
// own empty-string convention and runs UpdateRefsCAS. All-or-nothing:
// returns false if any compare failed.
func casRefs(ctx context.Context, ec *EtcdClient, updates []hookUpdate) bool {
	cas := make([]RefUpdate, 0, len(updates))

	for _, u := range updates {
		old := u.OldSha
		new := u.NewSha

		if isZeroSha(old) {
			old = ""
		}

		if isZeroSha(new) {
			new = ""
		}

		cas = append(cas, RefUpdate{
			Ref:    u.Ref,
			OldSha: old,
			NewSha: new,
		})
	}

	return ec.UpdateRefsCAS(ctx, cas)
}

// maybeInitHEAD picks the first newly-created branch as HEAD if no
// HEAD is set yet. Mirrors helper.go's first-push behaviour.
func maybeInitHEAD(ctx context.Context, ec *EtcdClient, updates []hookUpdate) {
	for _, u := range updates {
		if !strings.HasPrefix(u.Ref, "refs/heads/") {
			continue
		}

		if isZeroSha(u.NewSha) {
			continue
		}

		ec.PutHEADIfMissing(ctx, u.Ref)

		return
	}
}

func isZeroSha(sha string) bool {
	if len(sha) == 0 {
		return false
	}

	for _, c := range sha {
		if c != '0' {
			return false
		}
	}

	return true
}
