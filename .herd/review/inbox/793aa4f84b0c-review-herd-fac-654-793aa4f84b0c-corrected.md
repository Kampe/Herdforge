sha: 793aa4f84b0cd29737b9143804a631690a2cfa2d
branch: herd/fac-654
task: FAC-654
reviewer: review-herd-fac-654-793aa4f84b0c
reviewer-family: google
builder-family: openai
verdict: FAIL
reviewed-head: 793aa4f84b0cd29737b9143804a631690a2cfa2d
---
# Review Verdict for FAC-654

## Findings

The changes in `cmd/herd/envplan.go` and `cmd/herd/envplan_test.go` correctly bind environment plans to task-scoped graphs and properly implement exact run revision recovery for stale dispatches. The `environmentBindingFromAuthorities` function now validates task identities accurately when applying task-scoped dependency graphs. 

However, the changes introduce a test failure in `pkg/invariant` due to newly duplicated rule strings. Specifically, `TestNoNewDuplicatedRules` detects that the error messages `"dependency graph task snapshot returned empty revision"` and `"dependency graph task snapshot: scoped authority unavailable"` are now duplicated across `cmd/herd/envplan.go` and `cmd/herd/main.go`.

Additionally, there are unrelated failures in `pkg/harness`, `pkg/herdr`, `pkg/launch`, `pkg/mail`, `pkg/review`, `pkg/sync`, and `pkg/verifier`, as well as a failure in the `make lint` security gate due to missing `gosec` availability on the machine.

Given that `pkg/invariant` enforces that error strings should not be duplicated across files (to avoid diverging logic as seen in past issues), these strings must be extracted into a shared definition.

**Verdict:** REJECT (Requires fix for duplicated rules)

## Verification

**Commands Run:**
1. `git diff origin/main..HEAD`
2. `make lint`
3. `make test-unit`

**Outcomes:**

### `git diff origin/main..HEAD`
The diff correctly modifies `cmd/herd/envplan.go` to use `GraphForTask` rather than `GraphRevision` globally, validating that `saved.ID != task.ID || saved.Ref != task.Ref || (saved.ProjectID != "" && saved.ProjectID != task.ProjectID)` before yielding the graph for a task. Associated tests are updated.

### `make lint`
Failed (exit code 2) due to `make security`. The internal gate `./scripts/security-gate.zsh` failed because `gosec` is not correctly installed via `mise` in the current environment (`mise ERROR No version is set for shim: gosec`), leading `jq` to fail parsing empty results.

### `make test-unit`
Failed (exit code 1). While `cmd/herd` tests passed (`157.998s`), the `pkg/invariant` package correctly identified a regression introduced by this worktree:
```text
--- FAIL: TestNoNewDuplicatedRules (0.18s)
    duplicate_rule_test.go:56: new duplicated rule(s) — the same decision is now written in more than one place.
        This is the root cause of FAC-562, 565, 569, 571, 573 and 574: the copies diverge
        and a fix lands on only one of them.
        
          "dependency graph task snapshot returned empty revision"
            in: cmd/herd/envplan.go, cmd/herd/main.go
          "dependency graph task snapshot: scoped authority unavailable"
            in: cmd/herd/envplan.go, cmd/herd/main.go
```
(Note: The test run also exposed various unrelated test timeouts and failures in other packages like `launch` and `mail`.)
