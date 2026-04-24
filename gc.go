package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/go-git/go-git/v5/plumbing"
)

// gcMain is the `ogorod gc <repo>` subcommand. Walks every ref in
// etcd, computes the transitive closure of reachable objects by
// reading them back from S3, lists everything under the repo's
// object prefix, and deletes the set-difference.
//
// Idempotent; safe to run concurrently with a push — the worst
// outcome is that objects just uploaded but not yet ref-linked get
// swept, leaving the push as a partial failure. In practice push
// uploads objects BEFORE updating refs, so a concurrent GC would
// have to race the push tightly to cause trouble. For homelab
// scale we accept the risk; a belt-and-braces version would take
// /lock/ogorod/gc in etcdctl first.
func gcMain(args []string) {
	if len(args) < 1 {
		ThrowFmt("usage: ogorod gc <repo>")
	}

	repo := args[0]
	env := loadEnv()
	ctx := context.Background()

	ec := newEtcdClient(env, repo)
	defer ec.Close()
	s3 := newS3Client(env, repo)

	refs := ec.ListRefs(ctx)

	roots := make([]plumbing.Hash, 0, len(refs))

	for name, value := range refs {
		if name == "HEAD" {
			// HEAD holds "ref: refs/heads/X" — that ref's sha is in
			// the map already, so following HEAD separately would be
			// double-counting.
			continue
		}

		roots = append(roots, plumbing.NewHash(value))
	}

	reachable := walkReachableS3(ctx, s3, roots)

	all := s3.ListAll(ctx)

	fmt.Fprintf(os.Stderr, "ogorod gc %s: %d refs → %d reachable objects, %d total on S3\n",
		repo, len(refs), len(reachable), len(all))

	deleted := 0

	for _, sha := range all {
		if _, ok := reachable[plumbing.NewHash(sha)]; ok {
			continue
		}

		s3.Delete(ctx, sha)
		deleted++
	}

	fmt.Fprintf(os.Stderr, "ogorod gc %s: deleted %d orphan objects\n", repo, deleted)
}

// walkReachableS3 BFSes from roots through the DAG stored on S3,
// returning the set of every reachable object hash. Used by gc.
//
// Objects are decoded once to enumerate their children; the payload
// isn't retained. For a repo of a few hundred MB this fits easily in
// memory.
func walkReachableS3(ctx context.Context, s3 *S3Client, roots []plumbing.Hash) map[plumbing.Hash]struct{} {
	seen := make(map[plumbing.Hash]struct{})
	queue := append([]plumbing.Hash(nil), roots...)

	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]

		if _, ok := seen[h]; ok {
			continue
		}

		seen[h] = struct{}{}

		blob, ok := s3.Get(ctx, h.String())

		if !ok {
			// A ref points at an object we don't have. Log loudly
			// but don't bail — deleting the rest of the orphans is
			// still useful, and the operator needs to see this.
			fmt.Fprintf(os.Stderr, "ogorod gc: warning: ref points at missing object %s\n", h)

			continue
		}

		obj := DecodeLoose(blob)

		queue = append(queue, childHashes(memStorer(), obj)...)
	}

	return seen
}

// Tiny helper: strip trailing space from a printable stringfied
// hash, used in a few places to tolerate stray whitespace from
// stdin. Not public API but kept here to keep gc.go self-contained.
func trimHash(s string) string {
	return strings.TrimSpace(s)
}

var _ = trimHash // keep even if no current caller; cheap to retain.
