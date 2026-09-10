sha: ae13ef44a5031939c4880fc88424a5c0607a13de
branch: recovery/fac-785-op-readback
task: FAC-785
reviewer: review-fac-785-ae13ef44a503
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec
reviewed-head: ae13ef44a5031939c4880fc88424a5c0607a13de
---
## Findings and risk

Reviewed the exact full range `ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec..ae13ef44a5031939c4880fc88424a5c0607a13de` at the pinned candidate HEAD. The risk floor is R3: the classifier reports core orchestration/control-plane changes, production provider changes, and required different-family/security/high-risk gates. Graph status was current at 804 nodes, 11087 edges, and 27 files; its impact report identified 233 nodes within two hops and the new fence-op entry points.

The packet's `patch_id` is `c84e52bb625d8b36c805ad226ecef71dbd21de51`. The retained author verification digest is `sha256:5ce0d787c348c71af972994b9115c8142e6585e3a308d58a5951a729c7a61bad`; it is author evidence, not a native managed verification receipt. The packet records the accepted native launch as `forge-mender-fac785-dee-6163160c` with provider `opencode`, model `ollama-cloud/deepseek-v4-flash`, actual family `deepseek`, at `2026-09-09T23:11:17.551241Z`; the reviewer is OpenAI and different from the recorded `open-weight` builder family. The pinned tree is detached, but the candidate branch ref `recovery/fac-785-op-readback` decorates the exact reviewed commit. No live provider, board, or protected FAC655 operation was contacted.

Historical verdict evidence retained from the packet: prior independent review `1881` for `d65139ed` was FAIL for missing exact operation-bound upstream proof and lost applied-outbox identity; the retained `cb91` independent FAIL is not waived. The b7 correction and ae13 correction were reviewed as part of this full range.

1. **[High] Reconcile can report a mismatched outbox transition as `proven-applied`.** `cmd/herd/fence_ops.go:625-705` enumerates a provider-transition record and exposes its outbox identity, but the only consistency check at lines 669-678 compares local and broker receipts. It never compares the outbox operation kind/identity with the receipt's normalized `ExpectedStatus`, nor rejects a malformed/unparsed outbox binding before incrementing `WouldSettle`. I added one temporary hermetic public-CLI probe with an outbox `status:archive` record and an applied receipt whose expected status is `done`; on the exact candidate it returned `verdict: proven-applied`, `would_settle: 1`, and `settled: 0` (probe exit 1 only because work remained). This directly violates the packet's ae13 requirement to compare TaskID, OpID, normalized ExpectedStatus, and FenceToken across local/broker/outbox, and acceptance 1/2/4. If settlement authority is later wired in, this report can authorize closing the wrong existing transition without a provider write.

2. **[High] The public settlement positive path is absent.** `runFenceOpReconcile` exits at `cmd/herd/fence_ops.go:575-580` for every `--settle` invocation before opening stores or validating exact upstream proof. The implementation and help text explicitly call settlement unavailable. The CLI test only proves refusal; the concurrency/idempotence test calls the existing `ForceMarkApplied`/`ReconcileProviderTransitions` primitives directly and does not exercise an authorized public nondry settle. Therefore the card outcome "can settle ... only from authenticated operation-bound upstream proof" and acceptance 3-5 are not implemented or verified. This limitation is reported honestly per the packet; it is not treated as a successful settlement gate.

Residual risk remains around incomplete exact-field validation when one receipt field is blank: `checkReceiptConsistency` intentionally compares TaskID/OpID/status only when both sides are nonempty and FenceToken only when both are positive. The packet explicitly calls for zero/default versus real fence-token positive coverage, but no corresponding public test proves malformed blank identity is refused. The two High findings above are sufficient to deny approval.

## Tests run

- `go version` -> `go1.26.6 darwin/arm64`.
- `code-review-graph status --repo . --json` -> exit 0; 804 nodes, 11087 edges, 27 files, indexed at the reviewed HEAD.
- `code-review-graph detect-changes --repo . --base ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec --brief` -> exit 0; 11 changed files, 69 changed functions, overall graph risk 0.60, 43 reported test gaps.
- `code-review-graph impact --repo . --base ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec` -> exit 0; 468 directly changed nodes and 233 impacted nodes within two hops.
- `go test -count=1 -v ./cmd/herd -run '^TestFence'` -> exit 0; all fence/status/reconcile/authority/idempotence tests passed (`4.280s`).
- `go test -count=1 -v ./pkg/provider -run '^(TestFenceBrokerClientLookupOpFailClosed|TestValidateOpID|TestFAC785ReviewNullEmptyJSONErrorMustFailClosed|TestFAC785ReviewLookupOpReadErrorMustFailClosed)$'` -> exit 0 (`0.250s`).
- `go test -count=1 ./pkg/claim` -> exit 0 (`0.808s`).
- `go build ./cmd/herd` -> exit 0.
- `go vet ./cmd/herd ./pkg/provider ./pkg/claim` -> exit 0.
- `go run ./scripts/hermeticity/` -> exit 0.
- `make lint` -> exit 0; vet, nested vet, hermeticity, package inventory, security, dependency scan, and binary parity all passed.
- `go test -race -count=1 -timeout=300s ./cmd/herd ./pkg/provider ./pkg/claim` -> exit 1 after `249.708s`: `cmd/herd` failed its root Git-configuration guard; `pkg/provider` passed (`16.522s`) and `pkg/claim` passed (`6.390s`). The observed failure was `root git configuration guard failed: tests changed the root checkout's git config or remotes`.
- `make preflight` -> exit 2. Boundary, absolute-path, signal-literal, and merge-policy checks passed; the unbuilt `go run` binary reported stale provenance and the isolated branch is 30 commits behind `origin/main`. No source change was made to address this environment/repository-state gate.
- Final restored-source `go test -count=1 ./cmd/herd ./pkg/provider ./pkg/claim` -> exit 0 (`cmd/herd 224.443s`, `pkg/provider 12.696s`, `pkg/claim 1.790s`); `go build ./cmd/herd` and `git diff --check` also exited 0.

Mutation proof (one compiling independent control, restored afterward):

```text
Mutation: changed the task-mismatch comparison in checkReceiptConsistency from != to ==.
Command: go test -count=1 -v ./cmd/herd -run '^TestFenceOpTaskBindingMismatchRefusesAppliedAttribution$'
=== RUN   TestFenceOpTaskBindingMismatchRefusesAppliedAttribution
fence_ops_test.go:461: task binding mismatch on status must exit 1, got 0
--- FAIL: TestFenceOpTaskBindingMismatchRefusesAppliedAttribution (3.02s)
FAIL
MUTATION_RED_EXIT=1

Restored exact source comparison.
Command: go test -count=1 -v ./cmd/herd -run '^TestFenceOpTaskBindingMismatchRefusesAppliedAttribution$'
=== RUN   TestFenceOpTaskBindingMismatchRefusesAppliedAttribution
--- PASS: TestFenceOpTaskBindingMismatchRefusesAppliedAttribution (2.45s)
PASS
MUTATION_GREEN_EXIT=0
```

The temporary outbox mismatch probe was removed after its observed failure. Final `git status --porcelain` was empty, `git diff --check` was clean, and HEAD remained `ae13ef44a5031939c4880fc88424a5c0607a13de`.

## Author instructions

Repair the exact outbox binding gate before any settlement implementation is admitted: reject mismatched or unparsed outbox kind/status/op identity and prove the zero/default-token cases without allowing missing identity to pass. Then either implement the authenticated coordinator-only public `--settle` path through existing idempotent primitives with real hermetic positive/negative/concurrent tests, or keep it explicitly out of the claimed outcome and revise the card scope/acceptance with coordinator authority. Re-run the full-range independent review on the new candidate.
