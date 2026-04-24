# ogorod

Self-hosted git over `etcd` (refs, raft-CAS) + `MinIO` (objects,
erasure-coded). One Go binary, no daemon, piggybacks on the
quorum infrastructure already running on the cluster.

URL form: `ogorod://<repo-name>`.

## Why

Replaces github.com as the source-of-truth git host on a 3-node
homelab. Survives one-host loss without manual intervention —
etcd quorum picks ref winners, MinIO erasure-coding keeps the
objects readable. No additional service to babysit, no shared
filesystem, no Postgres-HA tail dependency.

## Bootstrap

```sh
# in MinIO (one-time):
mc mb minio/ogorod --ignore-existing

# environment for the helper (per client / per CI worker):
export OGOROD_ETCD_ENDPOINTS=lab1.nebula:8020,lab2.nebula:8020,lab3.nebula:8020
export OGOROD_S3_ENDPOINT=http://lab1.eth1:8012
export OGOROD_S3_ACCESS_KEY=...
export OGOROD_S3_SECRET_KEY=...
export OGOROD_S3_BUCKET=ogorod
```

## Use

```sh
# install the helper somewhere on $PATH (it must be named git-remote-ogorod)
go build -o /usr/local/bin/git-remote-ogorod .

# clone, push, pull as usual:
git clone ogorod://my-repo
git push ogorod://my-repo main
```

## Garbage collection

```sh
ogorod gc <repo>
```

Walks live refs from etcd, marks reachable objects, deletes the
orphan blobs from MinIO. Idempotent. Daily cron (job_scheduler)
recommended.

## Layout

See [CLAUDE.md](CLAUDE.md) for the storage layout, concurrency
semantics, and conventions. See [STYLE.md](STYLE.md) for code
style.
