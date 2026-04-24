package main

import (
	"context"
	"strings"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// EtcdClient scopes all ref operations to one repository under the
// /ogorod/refs/<repo>/ prefix. Open one per invocation, Close at
// end.
type EtcdClient struct {
	cli  *clientv3.Client
	root string
}

func newEtcdClient(env Env, repo string) *EtcdClient {
	cli := Throw2(clientv3.New(clientv3.Config{
		Endpoints:   env.EtcdEndpoints,
		DialTimeout: 10 * time.Second,
	}))

	return &EtcdClient{
		cli:  cli,
		root: "/ogorod/refs/" + repo,
	}
}

func (e *EtcdClient) Close() {
	e.cli.Close()
}

func (e *EtcdClient) refKey(ref string) string {
	return e.root + "/" + ref
}

// ListRefs returns a map of refname → sha for every ref in the
// repository (including HEAD, which stores "ref: refs/heads/X"
// rather than a sha — callers discriminate by key).
func (e *EtcdClient) ListRefs(ctx context.Context) map[string]string {
	resp := Throw2(e.cli.Get(ctx, e.root+"/", clientv3.WithPrefix()))

	out := make(map[string]string, resp.Count)

	for _, kv := range resp.Kvs {
		ref := strings.TrimPrefix(string(kv.Key), e.root+"/")
		out[ref] = string(kv.Value)
	}

	return out
}

// RefUpdate describes one intended ref mutation. OldSha == "" means
// "the ref must not exist yet" (CreateRevision == 0); NewSha == ""
// means "delete the ref".
type RefUpdate struct {
	Ref    string
	OldSha string
	NewSha string
}

// UpdateRefsCAS runs an atomic etcd Txn: every RefUpdate's OldSha
// is compared, every NewSha put/deleted. All-or-nothing.
//
// Returns true on success. On a compare-failure (concurrent update
// from another pusher, or non-fast-forward) returns false without
// panicking — the caller classifies per-ref since etcd doesn't tell
// us which compare failed.
func (e *EtcdClient) UpdateRefsCAS(ctx context.Context, updates []RefUpdate) bool {
	cmps := make([]clientv3.Cmp, 0, len(updates))
	ops := make([]clientv3.Op, 0, len(updates))

	for _, u := range updates {
		key := e.refKey(u.Ref)

		if u.OldSha == "" {
			cmps = append(cmps, clientv3.Compare(clientv3.CreateRevision(key), "=", 0))
		} else {
			cmps = append(cmps, clientv3.Compare(clientv3.Value(key), "=", u.OldSha))
		}

		if u.NewSha == "" {
			ops = append(ops, clientv3.OpDelete(key))
		} else {
			ops = append(ops, clientv3.OpPut(key, u.NewSha))
		}
	}

	resp := Throw2(e.cli.Txn(ctx).If(cmps...).Then(ops...).Commit())

	return resp.Succeeded
}

// PutRefForce writes a ref unconditionally — used for --force pushes,
// where the caller has accepted that existing state is overwritten.
func (e *EtcdClient) PutRefForce(ctx context.Context, ref, sha string) {
	Throw2(e.cli.Put(ctx, e.refKey(ref), sha))
}

// DeleteRef removes a ref unconditionally.
func (e *EtcdClient) DeleteRef(ctx context.Context, ref string) {
	Throw2(e.cli.Delete(ctx, e.refKey(ref)))
}

// PutHEADIfMissing sets /ogorod/refs/<repo>/HEAD to target only if
// no HEAD exists yet. Used on first push to a fresh repo to pick a
// default branch automatically.
func (e *EtcdClient) PutHEADIfMissing(ctx context.Context, target string) {
	key := e.refKey("HEAD")
	value := "ref: " + target

	Throw2(e.cli.Txn(ctx).
		If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
		Then(clientv3.OpPut(key, value)).
		Commit())
}
