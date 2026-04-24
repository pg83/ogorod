package main

import (
	"bytes"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/objfile"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// Git loose-object format is `<type> <size>\0<payload>`, zlib-
// compressed. go-git's objfile.Reader/Writer round-trip this form.
// We store the compressed bytes in S3 as-is — no re-encoding on
// retrieval, just stream the S3 body into the caller.

// EncodeLoose serialises an EncodedObject to the on-the-wire loose
// blob used on S3. Suitable for `S3Client.Put`.
func EncodeLoose(obj plumbing.EncodedObject) []byte {
	var buf bytes.Buffer
	w := objfile.NewWriter(&buf)

	Throw(w.WriteHeader(obj.Type(), obj.Size()))

	rd := Throw2(obj.Reader())
	defer rd.Close()

	Throw2(io.Copy(w, rd))
	Throw(w.Close())

	return buf.Bytes()
}

// DecodeLoose parses the loose-blob bytes back into an in-memory
// EncodedObject. Used on the fetch side after downloading from S3.
func DecodeLoose(blob []byte) plumbing.EncodedObject {
	rd := Throw2(objfile.NewReader(bytes.NewReader(blob)))
	defer rd.Close()

	typ, size, err := rd.Header()
	Throw(err)

	mo := &plumbing.MemoryObject{}
	mo.SetType(typ)
	mo.SetSize(size)

	w := Throw2(mo.Writer())
	Throw2(io.Copy(w, rd))
	Throw(w.Close())

	return mo
}

// childHashes returns every object hash referenced by obj's payload
// — parents + tree for commits, entries for trees, target for tags.
// Blobs have no children. The storer argument is used by go-git's
// decoders for any internal resolution; our MemoryObject inputs don't
// require it to hit the storer, but the type signatures demand one.
func childHashes(s storer.EncodedObjectStorer, obj plumbing.EncodedObject) []plumbing.Hash {
	switch obj.Type() {
	case plumbing.CommitObject:
		c := Throw2(object.DecodeCommit(s, obj))

		out := make([]plumbing.Hash, 0, 1+len(c.ParentHashes))
		out = append(out, c.TreeHash)
		out = append(out, c.ParentHashes...)

		return out

	case plumbing.TreeObject:
		t := Throw2(object.DecodeTree(s, obj))

		out := make([]plumbing.Hash, 0, len(t.Entries))
		for _, e := range t.Entries {
			out = append(out, e.Hash)
		}

		return out

	case plumbing.TagObject:
		t := Throw2(object.DecodeTag(s, obj))

		return []plumbing.Hash{t.Target}
	}

	return nil
}
