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

// uploadNewPacks finds *.pack files arriving in this push and ships
// them (pack first, then idx) to S3 if not already there.
//
// pre-receive runs while git-receive-pack still has the incoming
// objects in a *quarantine* directory (per `git help receive-pack`
// "Quarantine Environment"): the new pack lives at
// $GIT_OBJECT_DIRECTORY/pack/, *not* in <gitDir>/objects/pack/. The
// promotion to the main object store happens only after every hook
// returns 0. Scanning main back when we used to do it caught packs
// from the *previous* push (lag-by-one) — first push uploaded
// nothing, every subsequent push uploaded the prior push's pack,
// and the latest push's pack was always orphaned. Now we walk
// every object dir git tells us about — the quarantine one and
// any alternates — and pick up everything S3 doesn't have.
func uploadNewPacks(ctx context.Context, s3 *S3Client, gitDir string) {
	remote := s3.ListPacks(ctx)

	remoteSet := make(map[string]struct{}, len(remote))

	for _, n := range remote {
		remoteSet[n] = struct{}{}
	}

	for _, packDir := range packDirs(gitDir) {
		entries, err := os.ReadDir(packDir)

		if err != nil {
			continue
		}

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

			// Pack before idx: a fetcher that lists by .idx and
			// finds one must also find the matching .pack.
			s3.PutPack(ctx, base+".pack", packBytes)
			s3.PutPack(ctx, base+".idx", idxBytes)

			remoteSet[base] = struct{}{}

			fmt.Fprintf(os.Stderr, "ogorod-hook: uploaded %s.pack (%d bytes), %s.idx (%d bytes) from %s\n",
				base, len(packBytes), base, len(idxBytes), packDir)
		}
	}
}

// packDirs returns every objects/pack directory that might hold a
// pack relevant to this push:
//
//   - $GIT_OBJECT_DIRECTORY/pack — the quarantine dir where
//     receive-pack drops incoming packs before promotion;
//   - $GIT_ALTERNATE_OBJECT_DIRECTORIES (colon-separated) — typically
//     points at the main object store while quarantine is active;
//   - <gitDir>/objects/pack — the main store fallback when no
//     quarantine env is set (defensive — in practice receive-pack
//     always sets up quarantine on modern git).
func packDirs(gitDir string) []string {
	var out []string

	if v := os.Getenv("GIT_OBJECT_DIRECTORY"); v != "" {
		out = append(out, filepath.Join(v, "pack"))
	}

	if v := os.Getenv("GIT_ALTERNATE_OBJECT_DIRECTORIES"); v != "" {
		for _, p := range strings.Split(v, ":") {
			if p != "" {
				out = append(out, filepath.Join(p, "pack"))
			}
		}
	}

	out = append(out, filepath.Join(gitDir, "objects", "pack"))

	return out
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
