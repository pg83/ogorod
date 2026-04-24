# Style Guide

## Error handling

All error handling goes through `throw.go` — a thin panic/recover wrapper that turns Go's two-value error returns into exception-style flow.

### Primitives

- `Throw(err error)` — if `err != nil`, panic with an `*Exception`.
- `Throw2[T](val T, err error) T` — unwraps `(val, err)` returns; re-throws on error, otherwise returns `val`.
- `Throw3[T1,T2](v1 T1, v2 T2, err error) (T1, T2)` — three-value version.
- `ThrowFmt(format, args...)` — like `Throw(fmt.Errorf(...))` but unconditional; for raising our own errors.
- `Fmt(format, args...) *Exception` — construct an exception without throwing.
- `Try(cb func()) *Exception` — catch-all. Runs `cb`, converts `*Exception` panics into returned values, lets any other panic propagate.
- `(*Exception).Catch(cb)` — if non-nil, call `cb` with it. Fluent error handler at the boundary.
- `(*Exception).AsError() error` — interoperate with stdlib/3rd-party APIs that want an `error`.

### Rule

**No `if err != nil { return err }` in application code.** Wrap calls in `Throw2`/`Throw` instead:

```go
// BAD
f, err := os.Open(path)
if err != nil {
    return err
}

// GOOD
f := Throw2(os.Open(path))
```

Catches belong at boundaries:

- `main.go`: the top-level `Try(func(){...}).Catch(...)` prints the error and `os.Exit(1)`.
- The git-remote-helper command loop catches per-iteration so one bad request doesn't kill the helper.
- Each goroutine's entry function: wrap the body in `Try(...)` and log via `Catch` — otherwise a panic escapes the goroutine and kills the process.

### When `if err != nil` is allowed

Local, non-propagating uses are fine:

- **Filter**: `if err != nil { continue }` to skip a bad item in a loop.
- **Discriminate**: checking `errors.As`/`errors.Is` to recognise an expected case (e.g. an etcd compare-failed to detect concurrent push, or an S3 NoSuchKey to decide "object missing — fetch elsewhere").

The forbidden shape is a pure **pass-through** — `if err != nil { return err }` that does nothing except re-type the bubble. Use `Throw` for that.

### When a function should return `error`

Returning an `error` is fine — even encouraged — when the error is **part of the function's contract** and the caller is expected to branch on it, not just propagate it further:

- **Interface obligation**: e.g. `io.Reader.Read`, `flag.Value.Set(string) error`. The stdlib/3rd-party contract requires a returned `error`.
- **Domain signal that drives a branch**: e.g. `lookupRef` returning `ErrNotFound` so the caller can decide to create vs reject.

The distinction: does the caller do something *specific* with this error, or just `return err`? Latter — pass-through, use `Throw`.

## Formatting

### Blank lines around control blocks

Before and after `if`, `for`, `switch`, `select`, `go func`, `defer func` — add a blank line.

Exception: no blank line if the block is the first or last statement inside `{}`.

```go
func foo() {
    if cond {              // first stmt, no blank before
        return
    }
                           // blank after
    doThing()
                           // blank before for
    for _, x := range xs {
        use(x)
    }
}                          // for was last stmt, no blank after
```

### Blank lines before `return`

Always add a blank line before `return`.

Exception: no blank line if `return` is the first statement after `{`.

```go
func empty() int {
    return 0               // first stmt, no blank
}

func nonEmpty() int {
    x := compute()
                           // blank before return
    return x
}
```

### Logical grouping

Consecutive one-liners (`Throw*`, `defer`, `:=`, `=`) that form a single logical operation stay together without blank lines. Between separate logical operations — add a blank line.

## Project layout

All `.go` files live in the repo root. No `internal/`, no `cmd/`, no `pkg/`. The project is small; directory hierarchy would be overhead.

## Config

JSON only. No YAML, ever.

## Dependencies

- S3: `github.com/aws/aws-sdk-go-v2/service/s3`, configured for S3-compatible endpoints (works against MinIO with `UsePathStyle=true`).
- etcd: `go.etcd.io/etcd/client/v3` and `.../concurrency`.
- Git internals (loose object encode/decode, DAG walk, packfile parse): `github.com/go-git/go-git/v5` (and its sub-packages like `plumbing/format/objfile`).

No `os/exec` shell-outs. The whole binary is self-contained: no `git`, no `etcdctl`, no `mc` on the host's PATH required.
