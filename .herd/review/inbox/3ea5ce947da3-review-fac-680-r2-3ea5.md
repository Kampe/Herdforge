sha: 3ea5ce947da39b8c39590eb6e51442a2fbecab2b
branch: herd/fac-680
task: FAC-680
reviewer: review-fac-680-r2-3ea5
reviewer-family: anthropic
builder-family: openai
verdict: PASS
reviewed-head: 3ea5ce947da39b8c39590eb6e51442a2fbecab2b
---

## Scope

Fresh, independent round-2 review of candidate `3ea5ce947da3` on `herd/fac-680`,
base `9117a649319b949b422d6280711d9d8f914f57b8`. Prior verdict for this exact
SHA was not consulted; this review was built from scratch against the diff,
the surrounding package, and a live test run in the leased pool surface
(`.herd/pool/pool-01`, confirmed via `git rev-parse --show-toplevel`).

## Change

`pkg/signerboundary/attach_linux.go`: `peerPIDOfSocket` is rewritten to stop
hand-rolling the Linux `sockaddr_un` (raw `syscall.RawSockaddrUnix` byte
copy + `unsafe.Pointer` + `SYS_CONNECT`/`SYS_GETSOCKOPT`) and instead calls
`unix.Socket` (with `SOCK_CLOEXEC` added), `unix.Connect` with
`unix.SockaddrUnix{Name: socketPath}`, and `unix.GetsockoptUcred`. The
`unsafe` import is dropped. `pkg/signerboundary/attach_linux_test.go` is a
new file adding `TestPeerPIDOfSocketValidPaths` (filesystem ASCII path,
filesystem path with a high-bit byte, abstract `@name` socket, and a
107-byte maximum-length filesystem path) and
`TestPeerPIDOfSocketRefusesInvalidPaths` (missing socket, 108-byte overlong
path), asserting fail-closed (`0`) on the negative cases.

## Why this is a real fix, not a refactor

The old hand-rolled encoding wrote the raw path bytes into
`RawSockaddrUnix.Path` starting at index 0 unconditionally. For Linux
abstract-namespace sockets (name starting with `@`), the kernel requires
byte 0 of `sun_path` to be `NUL`, with the address length (not a NUL
terminator) delimiting the name. The old code left the literal `'@'` byte
in place and always passed `unsafe.Sizeof(addr)` (the full struct) as the
socklen, so it could never construct a correct abstract-socket address —
`peerPIDOfSocket` silently returned `0` for every abstract-namespace signer
socket. `x/sys/unix`'s `SockaddrUnix.sockaddr()` special-cases a leading
`'@'` (converts to a leading NUL and drops the trailing-NUL byte from the
length), so the new code is correct for both filesystem and abstract paths.

This also affects a real security path: `peerPIDOfSocket` backs
`discoverSignerPID` in `prove_sep.go`, which feeds `probeAttachDenied`'s
adversarial ptrace-attach-denial check on the signer process, and it backs
the `TerminateSigner` PID-recovery fallback in `rotate.go`. A signer bound
to an abstract socket previously could not be probed or torn down via this
path.

## Verification performed

- `git rev-parse --show-toplevel` confirms the surface resolves inside the
  leased pool slot (`.herd/pool/pool-01`), not the canonical shared
  checkout — isolation contract intact throughout.
- `git log/diff 9117a649..3ea5ce947` reviewed in full; only the two files
  above changed (93 insertions / 28 deletions), linear history, base is an
  ancestor of candidate.
- Read `golang.org/x/sys/unix@v0.46.0` (the version pinned in `go.mod`)
  `SockaddrUnix.sockaddr()` from the module cache to confirm the
  abstract-socket NUL-substitution and length handling described above.
- `go build ./...` — clean.
- `go vet ./pkg/signerboundary/...` — clean.
- `go test ./pkg/signerboundary/ -run TestPeerPIDOfSocket -v` on the
  candidate — all 6 subtests pass.
- `go test ./pkg/signerboundary/...` (full package, including the
  adversarial/boundary/hostile-worker suites that exercise
  `discoverSignerPID` transitively) — pass, 6.89s.
- Non-vacuity check per repo invariant (never trust a negative-assertion
  guard without seeing it fail): temporarily overwrote
  `attach_linux.go` in-place with the pre-fix (base-commit) content inside
  this leased pool worktree, reran the same test. Result:
  `TestPeerPIDOfSocketValidPaths/abstract` **fails** on the pre-fix code
  (`peerPIDOfSocket("@herd-fac-680-peer-pid") = 0, want <pid>`) while the
  other four subtests still pass — confirming the abstract-socket case is
  a genuine, previously-broken regression guard and not a vacuous
  assertion. Restored the file via `git checkout -- pkg/signerboundary/attach_linux.go`
  immediately after and confirmed `git status --porcelain` empty before
  and after.
- Confirmed no other production callers depend on the changed function's
  internals: `peerPIDOfSocket`/`processUIDPlatform` signatures are
  unchanged and match the `attach_other.go`/`attach_darwin.go`
  cross-platform contract; only `prove_sep.go` and `rotate.go` call
  `peerPIDOfSocket`, neither needed changes.
- Noted, non-blocking: `pkg/signerboundary/peer_creds_linux.go` still has
  an independent hand-rolled `SO_PEERCRED` `getsockopt` via raw
  `syscall.Syscall6` (`peerCredsFD`, used for already-accepted
  connections, a different code path than `peerPIDOfSocket`'s dial-based
  discovery). It wasn't touched by this candidate and is out of scope for
  FAC-680, but it's the same class of hand-rolled unsafe syscall this fix
  just eliminated elsewhere and would be a reasonable follow-up to
  consolidate onto `unix.GetsockoptUcred`.

## Verdict rationale

The change removes unsafe hand-rolled syscall plumbing in favor of the
well-tested `golang.org/x/sys/unix` equivalents, fixes a real
previously-silent failure mode for abstract-namespace signer sockets that
matters for the security-boundary adversarial probe and signer-termination
paths, and ships a test suite that is verified non-vacuous against the
pre-fix code. Build, vet, and the full package test suite are clean on the
candidate. No source was edited, no ledger/receipt/board state was
touched, and the pool worktree was left clean.

**PASS.**
