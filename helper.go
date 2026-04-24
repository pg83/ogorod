package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-git/go-billy/v5/osfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/memory"
)

const uploadWorkers = 32

// helperMain is the entry point git invokes for `ogorod://` remotes.
// argv is everything after the binary name:
//
//	os.Args[0]       git-remote-ogorod (any symlink of our binary)
//	os.Args[1]       remote name (e.g. "origin")
//	os.Args[2]       raw url ("ogorod://my-repo")
//
// We only need the URL; the remote name is informational.
//
// Protocol: read line-oriented commands on stdin, respond on stdout.
// Each command's response ends with a blank line. Docs:
// https://git-scm.com/docs/gitremote-helpers.
func helperMain(args []string) {
	if len(args) < 2 {
		ThrowFmt("usage: git-remote-ogorod <remote> <url>")
	}

	repo := parseURL(args[1])
	env := loadEnv()

	ec := newEtcdClient(env, repo)
	defer ec.Close()
	s3 := newS3Client(env, repo)

	gitDir := os.Getenv("GIT_DIR")

	if gitDir == "" {
		gitDir = ".git"
	}

	localStorer := filesystem.NewStorage(osfs.New(gitDir), cache.NewObjectLRUDefault())

	in := bufio.NewReader(os.Stdin)

	var pendingFetches []fetchReq
	var pendingPushes []pushReq

	for {
		line, err := in.ReadString('\n')

		if err != nil {
			// EOF is the normal termination.
			return
		}

		line = strings.TrimRight(line, "\n")

		switch {
		case line == "capabilities":
			fmt.Println("fetch")
			fmt.Println("push")
			fmt.Println()

		case line == "list", line == "list for-push":
			listRefs(ec)

		case strings.HasPrefix(line, "fetch "):
			parts := strings.SplitN(line[len("fetch "):], " ", 2)
			pendingFetches = append(pendingFetches, fetchReq{Sha: parts[0], Name: parts[1]})

		case strings.HasPrefix(line, "push "):
			pendingPushes = append(pendingPushes, parsePushReq(line[len("push "):]))

		case line == "":
			// End of a batch. Process whatever accumulated.
			if len(pendingFetches) > 0 {
				runFetches(context.Background(), s3, localStorer, pendingFetches)
				pendingFetches = nil
				fmt.Println()
			}

			if len(pendingPushes) > 0 {
				runPushes(context.Background(), ec, s3, localStorer, pendingPushes)
				pendingPushes = nil
				fmt.Println()
			}

		default:
			ThrowFmt("unknown helper command: %q", line)
		}
	}
}

func parseURL(url string) string {
	for _, prefix := range []string{"ogorod://", "ogorod::"} {
		if strings.HasPrefix(url, prefix) {
			s := strings.TrimPrefix(url, prefix)

			if s == "" {
				ThrowFmt("empty repo name in url %q", url)
			}

			return s
		}
	}

	ThrowFmt("invalid url %q: expected ogorod://<repo>", url)

	return ""
}

// listRefs prints every ref to stdout in helper protocol format:
//
//	<sha> <refname>
//	@<target> HEAD
//
// and terminates with a blank line.
func listRefs(ec *EtcdClient) {
	refs := ec.ListRefs(context.Background())

	// HEAD is a symbolic ref ("ref: refs/heads/X") rather than a sha;
	// emit with @ prefix so git knows to follow it. If HEAD isn't set
	// yet we just skip it — the client will manage without a default
	// branch hint.
	if head, ok := refs["HEAD"]; ok {
		target := strings.TrimPrefix(head, "ref: ")
		fmt.Printf("@%s HEAD\n", target)
		delete(refs, "HEAD")
	}

	for name, sha := range refs {
		fmt.Printf("%s %s\n", sha, name)
	}

	fmt.Println()
}

// --- fetch ---

type fetchReq struct {
	Sha  string
	Name string
}

func runFetches(ctx context.Context, s3 *S3Client, storer *filesystem.Storage, reqs []fetchReq) {
	// Each fetch request names a tip by sha; we need to pull that
	// object plus its transitive dependencies (commit → tree → blobs,
	// parents, …) until everything's locally present.
	want := make([]plumbing.Hash, 0, len(reqs))

	for _, r := range reqs {
		want = append(want, plumbing.NewHash(r.Sha))
	}

	downloaded := 0
	queue := want
	lastReport := time.Now()

	seen := make(map[plumbing.Hash]struct{})

	for len(queue) > 0 {
		h := queue[0]
		queue = queue[1:]

		if _, ok := seen[h]; ok {
			continue
		}

		seen[h] = struct{}{}

		// Already in local .git? Nothing to download — but still walk
		// its children so we don't miss tree/commit branches that are
		// partially present.
		obj, err := storer.EncodedObject(plumbing.AnyObject, h)

		if err == nil {
			queue = append(queue, childHashes(storer, obj)...)

			continue
		}

		blob, ok := s3.Get(ctx, h.String())

		if !ok {
			ThrowFmt("remote is missing object %s", h)
		}

		obj = DecodeLoose(blob)

		// Write into the local repo using a fresh storer-allocated
		// object so its sha-indexing matches go-git's invariants.
		local := storer.NewEncodedObject()
		local.SetType(obj.Type())
		local.SetSize(obj.Size())

		rd := Throw2(obj.Reader())

		func() {
			defer rd.Close()
			w := Throw2(local.Writer())
			defer w.Close()
			Throw2(io.Copy(w, rd))
		}()

		Throw2(storer.SetEncodedObject(local))
		downloaded++

		if time.Since(lastReport) >= 2*time.Second {
			fmt.Fprintf(os.Stderr, "ogorod: fetched %d objects (%d queued)...\n", downloaded, len(queue))
			lastReport = time.Now()
		}

		queue = append(queue, childHashes(storer, obj)...)
	}

	fmt.Fprintf(os.Stderr, "ogorod: fetched %d new objects (%d total reachable)\n",
		downloaded, len(seen))
}

// --- push ---

type pushReq struct {
	Force bool
	Src   string
	Dst   string
}

func parsePushReq(spec string) pushReq {
	p := pushReq{}

	if strings.HasPrefix(spec, "+") {
		p.Force = true
		spec = spec[1:]
	}

	parts := strings.SplitN(spec, ":", 2)

	if len(parts) != 2 {
		ThrowFmt("bad push refspec: %q", spec)
	}

	p.Src = parts[0]
	p.Dst = parts[1]

	return p
}

func runPushes(ctx context.Context, ec *EtcdClient, s3 *S3Client, storer *filesystem.Storage, reqs []pushReq) {
	// Snapshot the current remote refs. Two uses:
	//   1. Seed the local walker's "stop-at" set so we don't re-upload
	//      objects already on the remote (reachable from any existing
	//      tip, approximately).
	//   2. Feed the etcd CAS txn: each push's OldSha comes from here.
	remoteRefs := ec.ListRefs(ctx)

	stopAt := make(map[plumbing.Hash]struct{}, len(remoteRefs))

	for name, sha := range remoteRefs {
		if name == "HEAD" {
			continue
		}

		stopAt[plumbing.NewHash(sha)] = struct{}{}
	}

	// Ref deletions are pushes with Src == "". Handle them separately
	// from object-upload pushes, since no objects need to move.
	var casUpdates []RefUpdate
	var forcedUpdates []pushReq
	deleteResults := make(map[string]string)

	// 1. Upload objects for every creating/updating push.
	for _, r := range reqs {
		if r.Src == "" {
			// Delete: just queue the etcd op, no objects to push.
			casUpdates = append(casUpdates, RefUpdate{
				Ref:    r.Dst,
				OldSha: remoteRefs[r.Dst],
				NewSha: "",
			})

			deleteResults[r.Dst] = ""

			continue
		}

		localHash := Throw2(storer.Reference(plumbing.ReferenceName(r.Src))).Hash()

		uploadReachable(ctx, s3, storer, localHash, stopAt)

		if r.Force {
			forcedUpdates = append(forcedUpdates, pushReq{
				Src: localHash.String(),
				Dst: r.Dst,
			})
		} else {
			casUpdates = append(casUpdates, RefUpdate{
				Ref:    r.Dst,
				OldSha: remoteRefs[r.Dst],
				NewSha: localHash.String(),
			})
		}
	}

	// 2. CAS txn for non-forced updates + deletes.
	allOk := true

	if len(casUpdates) > 0 {
		allOk = ec.UpdateRefsCAS(ctx, casUpdates)
	}

	// 3. Forced updates — one unconditional Put per ref. These can't
	//    be bundled with the CAS txn because etcd Txn has no "put
	//    unconditionally" operator alongside If/Then/Else.
	for _, u := range forcedUpdates {
		ec.PutRefForce(ctx, u.Dst, u.Src)
	}

	// 4. First-push HEAD init. If the remote has no HEAD yet and we
	//    just pushed a branch, make that branch the default.
	if _, ok := remoteRefs["HEAD"]; !ok {
		for _, r := range reqs {
			if strings.HasPrefix(r.Dst, "refs/heads/") && r.Src != "" {
				ec.PutHEADIfMissing(ctx, r.Dst)

				break
			}
		}
	}

	// 5. Emit per-ref result to git.
	for _, r := range reqs {
		switch {
		case r.Src == "":
			fmt.Printf("ok %s\n", r.Dst)

		case r.Force:
			fmt.Printf("ok %s\n", r.Dst)

		case allOk:
			fmt.Printf("ok %s\n", r.Dst)

		default:
			fmt.Printf("error %s non-fast-forward (or concurrent update)\n", r.Dst)
		}
	}
}

// uploadReachable walks from root through the local object graph,
// uploading every object not in the stop set to S3. Stops at the
// provided remote-tip hashes as a heuristic to avoid re-uploading
// the bulk of already-known history.
//
// Worst case (force-push of unrelated history, or a stop-tip that
// isn't actually an ancestor) we re-upload some objects — S3 PUT
// is idempotent so this is a bandwidth cost only, not a correctness
// issue.
type uploadJob struct {
	hash plumbing.Hash
	blob []byte
}

func uploadReachable(ctx context.Context, s3 *S3Client, storer *filesystem.Storage, root plumbing.Hash, stopAt map[plumbing.Hash]struct{}) {
	seen := make(map[plumbing.Hash]struct{}, len(stopAt))
	for h := range stopAt {
		seen[h] = struct{}{}
	}

	// DAG walk is single-threaded (cheap, CPU-bound on small
	// objects) but feeds a pool of uploadWorkers doing the actual
	// S3 PUTs, since each PUT is one HTTP roundtrip and the LAN
	// latency is the bottleneck. Pool size 32 is empirical — MinIO
	// on a single-host LAN tops out somewhere in the 100s of
	// concurrent requests before saturating erasure-coded writes.
	jobs := make(chan uploadJob, uploadWorkers*2)

	var uploaded int64
	var firstErr atomic.Pointer[Exception]

	var wg sync.WaitGroup

	for i := 0; i < uploadWorkers; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()

			exc := Try(func() {
				for job := range jobs {
					s3.Put(ctx, job.hash.String(), job.blob)
					atomic.AddInt64(&uploaded, 1)
				}
			})

			if exc != nil {
				firstErr.CompareAndSwap(nil, exc)
			}
		}()
	}

	queue := []plumbing.Hash{root}
	lastReport := time.Now()

	for len(queue) > 0 {
		// Early exit if a worker has already failed — no point
		// enqueueing more work that's destined to fail too.
		if firstErr.Load() != nil {
			break
		}

		h := queue[0]
		queue = queue[1:]

		if _, ok := seen[h]; ok {
			continue
		}

		seen[h] = struct{}{}

		obj := Throw2(storer.EncodedObject(plumbing.AnyObject, h))
		blob := EncodeLoose(obj)

		jobs <- uploadJob{hash: h, blob: blob}

		if time.Since(lastReport) >= 2*time.Second {
			fmt.Fprintf(os.Stderr, "ogorod: uploaded %d objects (%d queued)...\n",
				atomic.LoadInt64(&uploaded), len(queue))
			lastReport = time.Now()
		}

		queue = append(queue, childHashes(storer, obj)...)
	}

	close(jobs)
	wg.Wait()

	if e := firstErr.Load(); e != nil {
		panic(e)
	}

	fmt.Fprintf(os.Stderr, "ogorod: uploaded %d objects (root %s)\n",
		atomic.LoadInt64(&uploaded), root)
}

// memStorer gives child-decode helpers something to satisfy their
// storer-argument typings when we don't actually want to cross-ref
// into another object store. Unused for now but useful if we ever
// need to DecodeCommit on bytes without touching real storage.
func memStorer() *memory.Storage {
	return memory.NewStorage()
}
