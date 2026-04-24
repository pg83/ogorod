package main

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/go-git/go-billy/v5"
	"github.com/go-git/go-billy/v5/memfs"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// repackMain implements `ogorod repack <repo>`:
//
//  1. Stream every loose object from S3 into an in-memory git
//     filesystem (memfs + filesystem.Storage).
//  2. Produce a packfile covering all of them via go-git's
//     PackfileWriter — it takes raw pack bytes on the write side
//     and builds the matching .idx on Close().
//  3. Upload pack + idx to <repo>/packs/.
//  4. Delete the loose objects the pack now subsumes.
//
// Ordering: list-at-start, delete-at-end. Any push that lands
// between those keeps its fresh loose objects — the repack only
// removes what it successfully packed.
func repackMain(args []string) {
	if len(args) < 1 {
		ThrowFmt("usage: ogorod repack <repo>")
	}

	repo := args[0]
	env := loadEnv()
	ctx := context.Background()

	s3 := newS3Client(env, repo)

	loose := s3.ListAll(ctx)

	if len(loose) == 0 {
		fmt.Fprintf(os.Stderr, "ogorod repack %s: nothing to do (no loose objects)\n", repo)

		return
	}

	fmt.Fprintf(os.Stderr, "ogorod repack %s: %d loose objects — downloading\n", repo, len(loose))

	scratchFS := memfs.New()
	scratch := filesystem.NewStorage(scratchFS, cache.NewObjectLRUDefault())

	hashes := make([]plumbing.Hash, 0, len(loose))

	for i, sha := range loose {
		blob, ok := s3.Get(ctx, sha)

		if !ok {
			fmt.Fprintf(os.Stderr, "ogorod repack %s: %s vanished mid-repack, skipping\n", repo, sha)

			continue
		}

		obj := DecodeLoose(blob)

		h := Throw2(scratch.SetEncodedObject(obj))
		hashes = append(hashes, h)

		if (i+1)%1000 == 0 {
			fmt.Fprintf(os.Stderr, "ogorod repack %s: downloaded %d/%d\n", repo, i+1, len(loose))
		}
	}

	fmt.Fprintf(os.Stderr, "ogorod repack %s: encoding pack (%d objects)\n", repo, len(hashes))

	// PackfileWriter: returns an io.WriteCloser that consumes raw
	// pack bytes. On Close() it generates the .idx too and commits
	// both into the underlying filesystem at objects/pack/pack-<sha>.
	pw := Throw2(scratch.PackfileWriter())

	enc := packfile.NewEncoder(pw, scratch, false /*useRefDeltas*/)
	packSha := Throw2(enc.Encode(hashes, 10 /*packWindow*/))

	Throw(pw.Close())

	// Read pack + idx out of the scratch filesystem.
	packName := "pack-" + packSha.String()

	packBytes := readFS(scratchFS, "objects/pack/"+packName+".pack")
	idxBytes := readFS(scratchFS, "objects/pack/"+packName+".idx")

	fmt.Fprintf(os.Stderr, "ogorod repack %s: pack=%d bytes idx=%d bytes, sha=%s\n",
		repo, len(packBytes), len(idxBytes), packSha)

	// Upload pack BEFORE idx: a concurrent fetcher that lists by
	// idx key and finds one must also find the matching pack.
	s3.PutPack(ctx, packName+".pack", packBytes)
	s3.PutPack(ctx, packName+".idx", idxBytes)

	fmt.Fprintf(os.Stderr, "ogorod repack %s: uploaded; deleting %d loose\n", repo, len(loose))

	for _, sha := range loose {
		s3.Delete(ctx, sha)
	}

	fmt.Fprintf(os.Stderr, "ogorod repack %s: done\n", repo)
}

// readFS pulls a file's full content out of a billy.Filesystem.
// Used to read the pack/idx that PackfileWriter just wrote into
// our scratch memfs.
func readFS(fs billy.Filesystem, path string) []byte {
	f := Throw2(fs.Open(path))
	defer f.Close()

	return Throw2(io.ReadAll(f))
}

// loadPacksIntoLocal downloads every remote pack (pack-<sha>.pack
// and .idx) into the local .git/objects/pack/ directory if not
// already present. After this, go-git's filesystem storer will
// find pack-stored objects natively — our fetch walker doesn't
// need a separate pack-lookup code path.
//
// Returns the count of newly-downloaded packs for logging.
func loadPacksIntoLocal(ctx context.Context, s3 *S3Client, gitDir string) int {
	packs := s3.ListPacks(ctx)

	packDir := gitDir + "/objects/pack"
	Throw(os.MkdirAll(packDir, 0o755))

	downloaded := 0

	for _, name := range packs {
		packPath := packDir + "/" + name + ".pack"
		idxPath := packDir + "/" + name + ".idx"

		_, perr := os.Stat(packPath)
		_, ierr := os.Stat(idxPath)

		if perr == nil && ierr == nil {
			continue
		}

		packBytes, ok := s3.GetPack(ctx, name+".pack")
		if !ok {
			continue
		}

		idxBytes, ok := s3.GetPack(ctx, name+".idx")
		if !ok {
			continue
		}

		Throw(os.WriteFile(packPath, packBytes, 0o644))
		Throw(os.WriteFile(idxPath, idxBytes, 0o644))

		downloaded++
	}

	return downloaded
}
