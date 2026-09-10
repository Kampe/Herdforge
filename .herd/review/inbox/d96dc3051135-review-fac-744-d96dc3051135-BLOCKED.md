sha: d96dc305113562e800b331b6728c466f06a09ebc
branch: recovery/fac-744-generation-recovery
task: FAC-744
reviewer: review-fac-744-d96dc3051135
reviewer-family: anthropic
builder-family: google
reviewed-base: 1656bb778e9afbcf03419a844e62253d699fe5df
reviewed-head: d96dc305113562e800b331b6728c466f06a09ebc
verdict: BLOCKED
---

## Review Verdict: BLOCKED

This review is **BLOCKED** due to missing production mutation proof of non-vacuity. The test assertion cannot be demonstrated to fail with the actual production mutation (moving `time.AfterFunc` timer arming before preparation).

## Analysis

The test checks:
```go
if _, hasDeadline := ctx.Deadline(); hasDeadline {
    t.Errorf("commandCtx has deadline during preparation: preparation must exclude command deadline")
}
```

The actual production timer placement (lines 313-320 in `pkg/verifier/verifier.go`):
```go
if commandTimeout > 0 {
    owned.commandStart = func() {
        commandTimer = time.AfterFunc(commandTimeout, func() {
            timedOut.Store(true)
            _ = cmd.Cancel()
        })
    }
}
```

## The Blocker

Moving `time.AfterFunc` before the `beforeCommandStart` call does **not** set a deadline on the context. The `time.AfterFunc` callback is independent of `context.Deadline()`:

- `time.AfterFunc(timeout, callback)` schedules a callback after duration; it does **not** set `ctx.Deadline()`
- `context.WithTimeout(ctx, timeout)` **does** set `ctx.Deadline()`

The test's assertion checks only the context deadline property, not timer state. Therefore:
- Moving `time.AfterFunc` before preparation → timer fires during prep, but `ctx.Deadline()` remains unset → **test still PASSES (incorrectly)**
- The test cannot catch this actual production bug

## Evidence

The test cannot distinguish between:
1. Timer armed before preparation (current test fails to catch)
2. Timer armed after preparation (what the code currently does, test passes)

Because the assertion only checks `ctx.Deadline()`, not timer scheduling state.

## Conclusion

The change is mechanically sound and addresses the timeout flake, but **the test does not provide non-vacuous proof** against the actual production mutation that would violate its own invariant. The test passes both with and without moving the `time.AfterFunc` timer before preparation.

This review cannot be admitted without:
1. A different assertion that checks actual timer state (not context deadline)
2. OR a production code change that sets a context deadline (not just schedules a timer)
3. OR explicit acknowledgment that this test only guards against deadline-during-prep, not timer-during-prep

---

## Metadata

patch_id: 39688cbd7f3a6cff7b0a84e2f1d26f298bc2b102
verification_digest: sha256:b00a0056a74a44c1ba986c08d893bbc4818d6bc7b121f18e279224bc10779326
full_range: 1656bb778e9afbcf03419a844e62253d699fe5df..d96dc305113562e800b331b6728c466f06a09ebc
blocker: time.AfterFunc timer movement does not set context.Deadline(); test assertion cannot fail with actual production mutation
