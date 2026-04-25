# ogorod — context for Claude

Tiny Go binary that exposes a 3-node MinIO + etcd cluster as a
plain HTTP git server. Vanilla `git clone http://lab:8035/<repo>.git`
and `git push` work — no client-side helper, no special URL scheme.
State lives in S3 (packs, EC quorum) + etcd (refs + push lock,
raft-CAS). Each lab host runs an instance; pushes serialize on an
etcd mutex, fetches don't.

A legacy `git-remote-ogorod` mode is still wired in for the URL
form `ogorod://<repo>` — same backend, client-side helper. Kept
during transition; will be removed once the server is established.

See [`README.md`](README.md) for the user-facing picture and
[`STYLE.md`](STYLE.md) for code conventions.

## Non-negotiable rules

- **Error handling via `Throw`/`Try`** (see `throw.go`). Forbidden: `if err != nil { return err }` pass-through. Use `Throw2(fn())`, `Throw(err)`, `ThrowFmt(...)`. Catch at boundaries (top-level main, helper command loop, goroutine entries).
- **Blank lines around every `if`/`for`/`switch`/`select` and before every `return`**, except when first/last in `{}`.
- **Config is JSON. Never YAML.**
- **Files live in the repo root** — no `internal/`, `cmd/`, `pkg/`.
- **No subprocess calls except to native `git`.** The server mode delegates the wire-protocol to `git http-backend` (CGI) — that's the whole point of "transparent server, vanilla client". Do not shell out to `etcdctl`, `mc`, or anything else; use go-git, etcd v3 client, aws-sdk-go-v2 directly.
- **Never truncate text for "readability".** No clipped logs, no `...(truncated)`, no `head -c` equivalents.

## Subcommands

One binary, multiple roles dispatched on argv[0] basename / argv[1]:

- `ogorod serve [--listen ADDR] [--cache-dir DIR]` — HTTP server fronting the cluster. Default `:8035`, default cache `/var/cache/ogorod`. Each request: parse repo from URL, take per-process mutex, on push also take etcd lease lock, sync local cache from S3+etcd, exec `git http-backend` CGI, stream response.
- `ogorod hook` — invoked by git as a `pre-receive` hook (the server installs a wrapper that re-execs us). Reads ref-updates from stdin, diffs local packs against S3, uploads new packs, runs CAS on refs, bumps version. Exits non-zero on any failure → git rejects the push.
- `git-remote-ogorod <remote-name> <url>` — legacy helper for `ogorod://` URLs. Implements `capabilities`, `list`, `fetch`, `push`. See <https://git-scm.com/docs/gitremote-helpers>.
- `ogorod gc <repo>` — garbage-collect orphan objects in MinIO. Walks live refs from etcd, computes reachable object set via go-git, deletes everything else under `<bucket>/<repo>/objects/`. Run nightly via job_scheduler cron.
- `ogorod repack <repo>` — admin: consolidate accumulated packs into one.

## Storage layout

### etcd (cluster `etcd_2` for now, dedicated cluster post-MVP)

```
/ogorod/refs/<repo>/heads/<branch>     → <40-hex sha>
/ogorod/refs/<repo>/tags/<tag>         → <40-hex sha>
/ogorod/refs/<repo>/HEAD               → "ref: refs/heads/main"
/ogorod/version/<repo>                 → monotonic int (bumped on every push)
/ogorod/lock/<repo>                    → etcd Mutex key (held during push)
```

One key per ref. Multi-ref push uses one `Txn` with N comparisons + N puts; all-or-nothing. Force-push = `Put` without compare.

### MinIO (single shared bucket `ogorod`)

```
ogorod/<repo>/packs/pack-<sha>.pack    → packfile (git native)
ogorod/<repo>/packs/pack-<sha>.idx     → packfile index v2
ogorod/<repo>/objects/<full_sha>       → loose object (legacy helper path
                                         only; server mode never produces
                                         these)
```

Flat — no two-byte prefix sharding (filesystem reason, not S3).

### Local cache (server mode)

Each lab host's server materializes a bare git repo per repo at
`/var/cache/ogorod/<repo>.git/`:

```
HEAD                   from etcd /ogorod/refs/<repo>/HEAD
config                 bare=true, http.receivepack=true, transfer.unpackLimit=0
packed-refs            from etcd ref map
objects/pack/*.pack    mirrored from S3
objects/pack/*.idx     mirrored from S3
hooks/pre-receive      shell wrapper that execs `ogorod hook`
.ogorod-version        last-synced etcd version key
```

Cache is ephemeral — it can be wiped at any time, the next request
re-materializes it from S3+etcd. SoT is always the cluster.

## Auth / endpoints

All from env. URL is endpoint-free; helper resolves cluster from env.

OGOROD_* takes precedence; standard AWS / etcdctl vars are fallbacks
so a box that already talks to MinIO via `aws`/`mc` and etcd via
`etcdctl` needs no extra configuration to use ogorod too.

| field       | primary                  | fallbacks                                  |
|-------------|--------------------------|--------------------------------------------|
| etcd        | `OGOROD_ETCD_ENDPOINTS`  | `ETCDCTL_ENDPOINTS`                        |
| S3 endpoint | `OGOROD_S3_ENDPOINT`     | `AWS_ENDPOINT_URL_S3`, `AWS_ENDPOINT_URL`  |
| S3 access   | `OGOROD_S3_ACCESS_KEY`   | `AWS_ACCESS_KEY_ID`                        |
| S3 secret   | `OGOROD_S3_SECRET_KEY`   | `AWS_SECRET_ACCESS_KEY`                    |
| S3 bucket   | `OGOROD_S3_BUCKET`       | (no standard fallback — bucket is ours)    |

URLs:

- Server mode (default for clients): `http://<lab-host>:8035/<repo>.git`
- Legacy helper: `ogorod://<repo>` (env-resolved cluster)

Repo name is the only payload, may contain slashes (`user/repo`).

ACL model: anyone who can reach the server (homelab wireguard) can
push. No per-repo permissions in MVP.

## Concurrent push semantics

Two layers protect against concurrent writers:

1. **Etcd lease mutex** — `ogorod serve` takes `/ogorod/lock/<repo>` for the duration of a push (info/refs advertisement → receive-pack POST). Holds across the whole push so two servers on different lab hosts can't interleave. Lease TTL 60s; auto-released if a server dies.
2. **Etcd CAS on each ref-update** — even with the lock, the actual write is `IF /ogorod/refs/<repo>/<ref> == <old_sha> THEN PUT <new_sha>` so a non-fast-forward push fails atomically.

For multi-ref push, all comparisons + puts in one Txn. If any
comparison fails, the hook fails → git-receive-pack rejects.

Force-push: not yet wired through the server hook (TODO). Legacy
helper supports `--force` via unconditional `PutRefForce`.

## Build / test

```
go build ./...        # produces ./git-remote-ogorod
go test ./...         # unit tests; integration testing means real etcd + MinIO
```

## Bootstrap (one-time)

```sh
# create the MinIO bucket (idempotent)
mc mb minio/ogorod --ignore-existing

# repo's first push creates etcd keys via CreateRevision == 0
# txn — no upfront repo registration needed.
```

## What's not done / future

- Force-push through the server hook (legacy helper supports it)
- HEAD updates via push (MVP only writes HEAD on create-empty-repo init)
- Per-repo ACLs (etcd RBAC keyed on path prefix)
- Webhook / post-receive triggers (run a CI on push)
- Read-only Forgejo overlay for browse / issues / PR UI
- Cache eviction (LRU on /var/cache/ogorod once it gets big)
- TLS — currently HTTP only; nginx in front if exposed beyond LAN
