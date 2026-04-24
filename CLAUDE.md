# ogorod — context for Claude

Tiny Go binary. A `git-remote-helper` that stores git refs in etcd
(via raft-CAS) and git objects in MinIO (erasure-coded, EC quorum).
Replaces github.com as a primary git host on a 3-node homelab while
matching the user's "no service that breaks on one-host loss" rule.

See [`README.md`](README.md) for the user-facing picture and
[`STYLE.md`](STYLE.md) for code conventions.

## Non-negotiable rules

- **Error handling via `Throw`/`Try`** (see `throw.go`). Forbidden: `if err != nil { return err }` pass-through. Use `Throw2(fn())`, `Throw(err)`, `ThrowFmt(...)`. Catch at boundaries (top-level main, helper command loop, goroutine entries).
- **Blank lines around every `if`/`for`/`switch`/`select` and before every `return`**, except when first/last in `{}`.
- **Config is JSON. Never YAML.**
- **Files live in the repo root** — no `internal/`, `cmd/`, `pkg/`.
- **No subprocess calls.** No `os/exec`, no shell-outs to `git`/`etcdctl`/`mc`. Use go-git, etcd v3 client, aws-sdk-go-v2 directly.
- **Never truncate text for "readability".** No clipped logs, no `...(truncated)`, no `head -c` equivalents.

## Subcommands

One binary, two roles dispatched on argv[0] basename / argv[1]:

- `git-remote-ogorod <remote-name> <url>` — invoked by git when a remote URL starts with `ogorod://`. Reads protocol commands on stdin, replies on stdout. Implements `capabilities`, `list`, `fetch`, `push`. See <https://git-scm.com/docs/gitremote-helpers>.
- `ogorod gc <repo>` — garbage-collect orphan objects in MinIO. Walks live refs from etcd, computes reachable object set via go-git, deletes everything else under `<bucket>/<repo>/objects/`. Run nightly via job_scheduler cron.

## Storage layout

### etcd (cluster `etcd_2` for now, dedicated cluster post-MVP)

```
/ogorod/refs/<repo>/heads/<branch>     → <40-hex sha>
/ogorod/refs/<repo>/tags/<tag>         → <40-hex sha>
/ogorod/refs/<repo>/HEAD               → "ref: refs/heads/main"
```

One key per ref. Multi-ref push uses one `Txn` with N comparisons + N puts; all-or-nothing. Force-push = `Put` without compare.

### MinIO (single shared bucket `ogorod`)

```
ogorod/<repo>/objects/<full_sha>       → zlib-compressed git object
                                         (loose-object format,
                                         per go-git/plumbing/format/objfile)
```

Flat — no two-byte prefix sharding (filesystem reason, not S3).

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

URL: `ogorod://<repo>` — repo name is the only payload.

ACL model: anyone who can write to MinIO can write to ogorod. No
per-repo permissions in MVP.

## Concurrent push semantics

Each ref update is an etcd Txn:

```
IF /ogorod/refs/<repo>/<ref> == <old_sha>     (or CreateRevision == 0 for new ref)
THEN PUT <new_sha>
ELSE reject → "non-fast-forward"
```

For multi-ref push, all comparisons + puts in one Txn. If any
comparison fails, the helper reports `error <ref> non-fast-forward`
back to git; the user retries (`git pull --rebase`, `git push`).

Force-push (`+ref:ref` or `--force`) skips the compare — does a
straight `Put`. Old objects become orphan, GC sweeps them later.

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

- HEAD updates via push (MVP only writes HEAD on create-empty-repo init)
- Per-repo ACLs (etcd RBAC keyed on path prefix)
- Pack-based fetch optimization for cold clone of large repos (loose-only is fine for our ~few-hundred-MB scale on LAN)
- Webhook / post-receive triggers (run a CI on push)
- Read-only Forgejo overlay for browse / issues / PR UI
