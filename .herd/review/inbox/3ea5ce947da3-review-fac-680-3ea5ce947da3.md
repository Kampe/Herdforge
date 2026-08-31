sha: 3ea5ce947da39b8c39590eb6e51442a2fbecab2b
branch: refs/herd/review/fac-680-3ea5
task: FAC-680
reviewer: review-fac-680-3ea5ce947da3
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: 3ea5ce947da39b8c39590eb6e51442a2fbecab2b
---

## Summary

`pkg/signerboundary/attach_linux.go`'s `peerPIDOfSocket` is rewritten to use
`golang.org/x/sys/unix` (`unix.Socket` + `unix.Connect` with `unix.SockaddrUnix`
+ `unix.GetsockoptUcred`) instead of hand-rolled `syscall.RawSockaddrUnix`
construction and raw `SYS_CONNECT`/`SYS_GETSOCKOPT` syscalls. `unsafe` import is
dropped. `SOCK_CLOEXEC` is added to the socket flags.

## Correctness

- The prior code built `RawSockaddrUnix` manually and never special-cased a
  leading `@` byte for Linux's abstract-socket namespace, so any abstract
  socket path was silently treated as a literal filesystem path and would fail
  to connect. `golang.org/x/sys/unix`'s `SockaddrUnix` marshaling performs the
  standard `@` → NUL translation (mirroring `net.UnixAddr`), so this change is
  a genuine correctness fix, consistent with the commit's stated intent ("let
  x/sys own Linux's filesystem/abstract sockaddr encoding and length
  validation").
- Max-path rejection is preserved: `x/sys/unix` returns `EINVAL` when
  `len(name) >= len(sa.raw.Path)` (108 on Linux), matching the old
  `len(path) >= len(addr.Path)` guard. The new 107-byte-boundary and
  108-byte-overlong test cases confirm this at both edges.
- `SO_PEERCRED` retrieval via `unix.GetsockoptUcred` is the standard typed
  wrapper for the same getsockopt call the old code performed manually;
  `cred.Pid` usage is unchanged.
- `SOCK_CLOEXEC` addition closes a minor fd-leak-across-exec window that
  existed in the old code; it does not change any documented behavior of this
  function since the fd is already closed via `defer` before `peerPIDOfSocket`
  returns.
- Checked both callers (`prove_sep.go:discoverSignerPID`,
  `rotate.go:TerminateSigner`) — neither depends on removed behavior, and
  `attach_other.go`/`attach_darwin.go` (non-Linux build tags) are untouched.

## Tests

`attach_linux_test.go` is new and covers `peerPIDOfSocket` directly:
filesystem ASCII path, filesystem path with a high-bit byte, an abstract
(`@`-prefixed) socket, the exact 107-byte maximum filesystem path length, a
missing socket (fail-closed to 0), and a 108-byte overlong path (fail-closed
to 0). This is the right set of edge cases for a sockaddr-encoding rewrite and
directly exercises the abstract-socket fix the commit claims. Good use of
`t.TempDir()`/`t.Cleanup()`; no flakiness concerns (real listener + real
connect, no timing assumptions).

## Residual risk

None material. Could not execute `go build`/`go test` in this environment (no
`go` toolchain on PATH), so this review is based on static reading of the
diff, the `x/sys/unix` `SockaddrUnix` marshaling behavior, and cross-checking
callers — not a live test run. The change is small, self-contained to one
Linux-only file plus its new test file, and the test additions are consistent
with and would catch a regression in the claimed fix.
