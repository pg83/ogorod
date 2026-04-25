# ogorod

Self-hosted git over `etcd` (refs, raft-CAS) + `MinIO` or any other
S3-compatible object store (packs). One Go binary covers four
roles: a server, a `pre-receive` hook, a remote-helper, and a
couple of admin commands.

## What it gives you

A git host that

- has no Postgres / no shared filesystem / no second daemon to
  babysit beyond the etcd + S3 you already run;
- survives any single-host loss without manual intervention (etcd
  quorum picks ref winners, S3 erasure coding keeps packs readable);
- speaks vanilla smart-HTTP to clients — `git clone` and `git push`
  with no special URL scheme, no client-side helper.

## Three deployment modes

Pick one based on how much of git's wire protocol you want to own:

| Mode               | Run                     | What clients see                  | Server-side process |
|--------------------|-------------------------|-----------------------------------|---------------------|
| **fat server**     | `ogorod serve`          | `http://host:8035/<repo>.git`     | yes, our HTTP layer |
| **thin server**    | `ogorod serve-thin`     | `http://host:8038/<repo>.git`     | yes, our HTTP layer + native git-http-backend |
| **client helper**  | `git-remote-ogorod`     | `ogorod://<repo>`                 | no, all logic on the client |

Fat and thin both expose vanilla smart-HTTP. Helper mode is for
read/write from clients without standing up any server at all —
the helper running on each developer's machine talks straight to
etcd + S3.

The two server modes can run side-by-side against the same backend
(different ports, different cache dirs); fetches and pushes via
either land in the same place.

## Backend setup (one-time)

```sh
# In your S3, create the bucket. ogorod will not create it for you.
mc mb minio/ogorod
```

etcd needs nothing — keys appear under `/ogorod/...` on first push.
The first push to a new repo creates `/ogorod/refs/<repo>/...` via
a CreateRevision==0 transaction; subsequent pushes use CAS.

## Environment

Every mode reads the same env. `OGOROD_*` takes priority;
`AWS_*` / `ETCDCTL_*` are accepted as fallbacks so a host already
configured for `aws-cli` / `mc` / `etcdctl` works with no extra
configuration.

| Field            | Primary                 | Fallbacks                                            |
|------------------|-------------------------|------------------------------------------------------|
| etcd endpoints   | `OGOROD_ETCD_ENDPOINTS` | `ETCDCTL_ENDPOINTS`                                  |
| S3 endpoint      | `OGOROD_S3_ENDPOINT`    | `OGOROD_S3_URL`, `MC_HOST_<alias>`, `AWS_ENDPOINT_URL_S3`, `AWS_ENDPOINT_URL` |
| S3 access key    | `OGOROD_S3_ACCESS_KEY`  | `OGOROD_S3_URL`, `MC_HOST_<alias>`, `AWS_ACCESS_KEY_ID` |
| S3 secret key    | `OGOROD_S3_SECRET_KEY`  | `OGOROD_S3_URL`, `MC_HOST_<alias>`, `AWS_SECRET_ACCESS_KEY` |
| S3 bucket        | `OGOROD_S3_BUCKET`      | defaults to `ogorod`                                 |

`OGOROD_S3_URL` and `MC_HOST_<alias>` are compound forms —
`http[s]://access:secret@host[:port]` — so a single env var carries
endpoint + credentials in one go.

## Mode 1 — fat server (`ogorod serve`)

```sh
ogorod serve [--listen :8035] [--cache-dir /var/cache/ogorod]
```

Built-in HTTP server. Each request: parse repo, materialize a bare
git repo from etcd + S3 into the cache dir, exec `git http-backend`
as a CGI subprocess, stream the response. Push paths additionally
hold an etcd lease mutex so concurrent pushes from different
servers serialize.

Clients:

```sh
git clone http://host:8035/<repo>.git
git push  http://host:8035/<repo>.git main
```

Run one instance per host; all instances share state via etcd + S3.
HA model: any instance can serve any request.

## Mode 2 — thin server (`ogorod serve-thin`)

```sh
ogorod serve-thin [--listen :8038] [--cache-dir /var/cache/ogorod-thin] [--bin-dir /var/run/ogorod-thin/bin]
```

Same external interface as the fat server, but the cluster-sync
logic moves out of the HTTP handler and into per-binary wrappers
around `git-upload-pack` / `git-receive-pack`:

1. On startup, populates `--bin-dir` as a "shadow" git exec-path:
   symlinks every helper from `git --exec-path`, then overlays our
   own wrappers for `upload-pack` and `receive-pack`.
2. The HTTP handler exec's `git http-backend` with `GIT_EXEC_PATH`
   pointed at the shadow dir, so the backend's launches of
   `git_cmd=1` upload-pack / receive-pack pick up our wrappers.
3. Each wrapper re-exec's `ogorod wrap {upload-pack|receive-pack}`,
   which runs cluster-sync and then `syscall.Exec`s the real git
   binary — `GIT_EXEC_PATH` is dropped before the syscall so the
   real git falls back to its compiled-in exec-path and doesn't
   loop into our wrapper again.

Net effect: fetches and pushes pay the sync cost lazily, exactly
when a real protocol op fires. Useful as an A/B comparison against
the fat server on the same backend.

Clients use the same vanilla smart-HTTP URL, just the new port:

```sh
git clone http://host:8038/<repo>.git
```

## Mode 3 — client helper (`git-remote-ogorod`)

No server at all. Each developer's machine talks straight to etcd
and S3 via a remote-helper.

```sh
go build -o /usr/local/bin/git-remote-ogorod .

git clone ogorod://my-repo
git push  ogorod://my-repo main
```

git invokes `git-remote-ogorod` whenever it sees `ogorod://...`.
The helper reads the protocol on stdin, talks to etcd + S3, replies
on stdout. Same backend semantics as the server modes — same refs,
same packs, same CAS — just no HTTP layer in between. Pick this for
a small, trusted team where nobody wants to run anything central.

The three modes interoperate: a developer using the helper can
push, then someone else can clone via `http://host:8035` and see
the same data.

## Hook (`ogorod hook`)

Both server modes install a `pre-receive` hook into every
materialized cache repo that re-exec's `ogorod hook`. It

1. Reads ref-updates from stdin (the standard pre-receive
   protocol).
2. Diffs the local cache's `objects/pack/` against S3 to find
   newly-arrived packs from this push.
3. Uploads them (pack first, then idx).
4. Runs CAS on each ref update in etcd, all-or-nothing in one
   transaction.
5. Bumps `/ogorod/version/<repo>` so the other servers re-sync on
   their next request.
6. Exits non-zero on any failure — git rejects the push atomically.

You don't run this directly; the servers wire it up.

## Admin commands

```sh
ogorod gc     <repo>   # delete S3 blobs unreachable from current refs
ogorod repack <repo>   # legacy: collapse loose-object area into one pack
```

`gc` walks live refs from etcd, computes the reachable object set
via go-git, and deletes everything else under the repo's S3
prefix. Idempotent. A nightly cron is recommended.

`repack` is mostly historical — early helper mode wrote loose
objects on push; modern push always writes packs.

## Concurrent push semantics

Two layers protect against concurrent writers:

1. **Etcd lease mutex** — `ogorod serve{,-thin}` takes
   `/ogorod/lock/<repo>` for the duration of a push (info/refs
   advertisement → receive-pack POST). Holds across the whole push
   so two servers on different hosts can't interleave. Lease TTL
   60 s; auto-released if a server dies mid-push.
2. **Etcd CAS on each ref-update** — even with the lock held, the
   actual write is `IF /ogorod/refs/<repo>/<ref> == <old_sha>
   THEN PUT <new_sha>`, so a non-fast-forward push fails atomically.

For multi-ref push, all comparisons + puts go in one Txn. If any
comparison fails, the hook fails → git-receive-pack rejects.

## Storage layout

S3:

```
<bucket>/<repo>/packs/pack-<sha>.pack
<bucket>/<repo>/packs/pack-<sha>.idx
<bucket>/<repo>/objects/<full_sha>     (legacy helper path only)
```

etcd:

```
/ogorod/refs/<repo>/<refname>          → <40-hex sha>
/ogorod/refs/<repo>/HEAD               → "ref: refs/heads/main"
/ogorod/version/<repo>                 → monotonic int (bumped on push)
/ogorod/lock/<repo>                    → etcd Mutex key (held during push)
```

## Build

```sh
go build ./...
```

Produces `./ogorod`. The legacy helper is the same binary invoked
under the name `git-remote-ogorod` — symlink or hardlink the
binary as appropriate:

```sh
ln -s ogorod /usr/local/bin/git-remote-ogorod
```

Single binary, dispatched on argv[0] basename / argv[1]:

```
git-remote-ogorod <remote> <url>      # legacy helper protocol
ogorod serve [...]                     # fat HTTP server
ogorod serve-thin [...]                # thin HTTP server
ogorod wrap {upload-pack|receive-pack} # called by the thin server's wrappers
ogorod hook                            # pre-receive hook
ogorod gc <repo>                       # garbage collect
ogorod repack <repo>                   # consolidate loose into a pack
```

## See also

- [`CLAUDE.md`](CLAUDE.md) — internals, conventions, what's not done.
- [`STYLE.md`](STYLE.md) — code style.
