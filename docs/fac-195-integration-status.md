# FAC-195 integration status

The compiled attempt-budget control ships and is runnable today. One
acceptance criterion — binding lane/session identity to a *proven* exec
session — is BLOCKED on FAC-193, which has no code anywhere. This document
names the exact seam rather than faking it.

## What is delivered and works today

- `pkg/cmdauth`: the durable, fail-closed attempt budget. An immutable
  command packet (command ID, canonical command hash, max attempts, issuing
  authority, lane/session, failure disposition) is durably authorized; an
  attempt is consumed atomically **before** process creation; a spent budget
  or a failed stop-on-first-failure token refuses every later attempt until a
  distinct newly authorized command ID arrives. Receipts are append-only,
  enforced by SQLite triggers rather than by convention.
- `herd command authorize | run | receipts`: a real production caller. A
  guarded command is issued by root/coordinator and executed only through
  `herd command run`, which refuses with a distinct exit code (77) when the
  boundary says no, before any child process exists.
- The FAC-151 pattern is reproduced hermetically in both
  `pkg/cmdauth/cmdauth_test.go` (counting spawn) and
  `cmd/herd/command_cli_test.go` (four separate OS processes). No FAC-151
  native-tag fixture and no host signal is used anywhere in these tests.

## BLOCKED: proven lane/session identity (FAC-193)

**Criterion:** "Integrate exact process/session ownership with FAC-193."

Lane and session identity are **caller-asserted** at this boundary. A worker
presenting `--lane worker-a --session S` is checked for equality against what
the authorization binds, which stops a token issued to one lane from being
presented by another *lane*, but does not prove the presenting **process** is
that lane's exec session.

**FAC-193 merged during this rebase** (`92a59a0`, `pkg/cmdsession`), so the
earlier version of this document — which said its branch was empty — is
superseded. The blocker did not disappear; it changed shape, and the new
shape is narrower and worth stating exactly.

`pkg/cmdsession` is a durable command-session authority keyed by
`(CoordinatorSession, ToolCallID)` and bound to an exact PID + start token.
It is the right authority. Two things still prevent binding to it here:

**1. It has no production writer.** `Store.Register` is never called outside
its own tests, so the table a `ProveOwner` would query is empty in
production:

```
$ grep -rnE '\.Register(Detached)?\(' --include='*.go' . | grep -v _test.go
pkg/mutationprobe/lifecycle.go:232 ...   # unrelated store

$ grep -n '^func runCommandSessions' cmd/herd/commandsessions.go
runCommandSessions / ...Status / ...Reconcile      # read + reconcile only
```

**2. Nothing conveys a tool-call identity to the executing process.** There is
no env var or flag by which `herd command run` could learn its own
`(CoordinatorSession, ToolCallID)`:

```
$ grep -rn 'HERD_TOOL_CALL\|HERD_COORDINATOR_SESSION\|TOOL_CALL_ID' --include='*.go' .
(no output)
```

Wiring `ProveOwner` to `cmdsession` today would therefore do one of two bad
things: fail closed on every lookup miss and refuse all execution, bricking
the boundary; or treat absence as success, which proves nothing while looking
rigorous. Neither is worth shipping, so the seam stays declared and unbound.

**What unblocks it:** a production caller that registers a command session at
spawn, plus a channel (env var is the obvious one) carrying the tool-call
identity into the executed process. Once both exist, `ProveOwner` looks up the
session and confirms the caller is its registered PID/StartToken.

FAC-172's merged `herdr.GetHostedPaneIdentity` / `herdr.AssertHostedPaneUID`
do not substitute: they prove a *pane's* processes run under the builder UID,
not that a process is lane L session S's executor.

**The symbols to bind to**, named exactly, now on `origin/main` at
`pkg/cmdsession/store.go`:

```go
func (s *Store) Get(key cmdsession.Key) (*cmdsession.Receipt, error)

type Key struct {
	CoordinatorSession string
	ToolCallID         string
}

type Identity struct {
	Key
	PID, ParentPID int
	StartToken     string
	CommandDigest  string
	WorkingDir     string
	...
}
```

`OwnerProver.ProveOwner` is the adapter point: an implementation looks up the
live command session and confirms the calling process is its registered
`PID`/`StartToken`. Note for whoever integrates — the two boundaries are keyed
differently on purpose and the adapter must bridge them: `cmdauth` binds
`(lane, sessionID)` while `cmdsession` binds `(CoordinatorSession, ToolCallID)`.
`cmdsession.Identity.CommandDigest` + `WorkingDir` are the same pair
`cmdauth.CanonicalHash(dir, argv)` covers, so the two hashes should be
reconciled at that time rather than computed twice from different rules.

The packages are adjacent but not duplicative, in the same way `cmdsession`
itself defers to `pkg/toolchild`: `cmdsession` owns command-session *lifecycle
and reaping*, `cmdauth` owns *whether an attempt may be spent at all*. Neither
tears down or authorizes on the other's behalf.

**The seam, exactly as it is waiting to be filled:**

```go
// pkg/cmdauth/cmdauth.go
type OwnerProver interface {
	ProveOwner(ctx context.Context, lane, sessionID string) error
}

// Store.Prover is consulted inside the SAME transaction that consumes the
// attempt. A nil Prover means caller-asserted identity. A non-nil Prover
// that returns an error refuses the attempt with ErrOwnerUnproven, consuming
// nothing and spawning nothing.
```

Once the two prerequisites above are met, wiring means implementing that one
method against `cmdsession` and assigning it to `Store.Prover` in
`cmd/herd/command.go` — no change to this package should be needed.
`TestOwnerProverSeamFailsClosed` already pins the contract from both sides
(refusal consumes no attempt and spawns nothing; acceptance leaves the token
usable), so the adapter has a test to land against on day one.

## Known limit, in scope to state and out of scope to fix here

This boundary governs commands routed **through** it. It cannot stop an agent
that ignores `herd command run` and shells out directly — nothing inside a
single trust domain can, because the agent holds the same UID and the same
shell. Closing that requires the harness to make the boundary the *only*
reachable exec path, which is the least-privilege sandbox work on the
unmerged `task/fac-133` branch, not this ticket's surface.

What FAC-195 removes is the specific failure mode the incident had: an
authorization that existed only as prompt wording, where a compliant executor
had nothing to consult and a non-compliant one left no durable evidence.
Every attempt against this boundary — permitted, refused, or entirely
unauthorized — is now a receipt.

## Related tickets not merged at the time of writing

Rechecked when this branch was rebased onto `4235798` (main moved three times
during this rebase; every claim below was re-verified against that final base,
not inherited from an earlier one):

| Ticket | State |
| --- | --- |
| FAC-193 | **merged** (`92a59a0`, `pkg/cmdsession`). Owns the ownership seam above, but has no production writer and no tool-call-identity channel yet, so the seam stays unbound — see the BLOCKED section for the exact evidence. Its CLI is `herd commands`; this ticket's is `herd command` — distinct switch cases, no shared helper names. |
| FAC-176 | no commits; branch `herd/fac-176` is empty. |
| FAC-133 | unmerged work on `task/fac-133` (least-privilege sandbox) — see the known limit above. |
| FAC-183 | merged (`14b16c7`, `85fbb24`); nothing here depends on it. |
| FAC-169 | merged (`e33f8ae`, `pkg/signerboundary`). Independent concern — signs reviewer verdicts, does not gate command execution. |
| FAC-172 | merged (`9334266`..`15642b0`, hosted-UID capability gate). See above: pane-UID proof, not lane/session executor identity. |
| FAC-190 | merged (`ee63761`..`f004805`, `pkg/confinement`). OS write denials at dispatch; complementary to, and does not supersede, this attempt budget. |
