sha: cb91dc3317ef273f9d006056dc61aacca0c2b3a7
branch: recovery/fac-785-op-readback
task: FAC-785
reviewer: review-fac-785-cb91dc3317ef
reviewer-family: openai
builder-family: google
verdict: FAIL
reviewed-base: ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec
reviewed-head: cb91dc3317ef273f9d006056dc61aacca0c2b3a7
---

# Review FAC-785

FAIL. The exact full range is pinned and the core status/reconcile paths are read-only in the exercised fixtures. The candidate still violates the card's explicit fail-closed HTTP-200 JSON-error contract in two ways.

## Identity and scope

- task_ref: FAC-785
- candidate_sha: cb91dc3317ef273f9d006056dc61aacca0c2b3a7
- reviewed_base: ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec
- reviewed_head: cb91dc3317ef273f9d006056dc61aacca0c2b3a7
- patch_id: 4144f7ad30cb215bfd709e4e978246f923ec7b35
- verification_digest: sha256:dd32f1aad1a826dec9168ffbeb0a0428bfc81e4a3b6d10c9bec495359be32a7c
- reviewer: review-fac-785-cb91dc3317ef
- reviewer family: openai (Codex gpt-5.6-luna)
- builder family: google, preserved from the accepted launch ledger. The packet records native primary lineage as actual DeepSeek followed by native Google Lazer Gemini3.7Flash; this verdict does not re-derive or overwrite the ledger value.
- The review surface was detached at the exact candidate, with the candidate branch ref recorded as recovery/fac-785-op-readback.
- Protected FAC655 operation 9eedc96717a346bfc65786e709c3a9df was never referenced or called.

## Findings

### 1. [High] HTTP-200 `error:null` and `error:""` are explicitly accepted

`pkg/provider/fence_broker_client.go:390-407` makes `rejectJSONErrorBody` skip both the JSON `null` value and the empty string:

```go
if rawStr != "" && rawStr != "null" && rawStr != `""` {
```

Therefore a 200 response such as `{"error":null,"applied":true,"ambiguous":false,"op_id":"aa","task_id":"t1"}` can be accepted by `LookupOp` as applied evidence. This directly violates the FAC-785 packet's requirement that raw HTTP-200 JSON error bodies fail closed, including the null/empty edge. It also affects the existing status/comment mutation response checks because the helper is shared. The correction must treat the presence of an `error` key as an error for every JSON value, including null and empty string, before interpreting the rest of the body.

Watched compiling RED control, run against this candidate:

```text
go test ./pkg/provider -run '^TestFAC785ReviewNullEmptyJSONErrorMustFailClosed$' -count=1
go version go1.26.6 darwin/arm64
--- FAIL: TestFAC785ReviewNullEmptyJSONErrorMustFailClosed (0.00s)
    fac785_review_tmp_test.go:8: HTTP 200 {"error":null} must fail closed
FAIL
FAIL	github.com/Kampe/Herdforge/pkg/provider	0.256s
FAIL
exit=1
```

### 2. [High] `LookupOp` discards HTTP response-body read errors

`pkg/provider/fence_broker_client.go:255-260` performs `body, _ := io.ReadAll(resp.Body)`. A transport can return a valid-looking JSON prefix together with a read error; the ignored error is then lost and the bytes are parsed as authenticated operation evidence. That is an unavailable/truncated upstream response being treated as success, contrary to the acceptance requirement that unavailable upstream fail closed. The body read error must be propagated before error-body or receipt interpretation.

Watched compiling RED control, run against this candidate:

```text
go test ./pkg/provider -run '^TestFAC785ReviewLookupOpReadErrorMustFailClosed$' -count=1
--- FAIL: TestFAC785ReviewLookupOpReadErrorMustFailClosed (0.00s)
    fac785_review_tmp_test.go:39: body read error must fail closed
FAIL
FAIL	github.com/Kampe/Herdforge/pkg/provider	0.245s
FAIL
exit=1
```

Both temporary controls were deleted after each RED run. Final `git status --porcelain=v1` was empty and the tracked source was unchanged.

## Source and graph review

- Full source range reviewed: `ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec..cb91dc3317ef273f9d006056dc61aacca0c2b3a7`.
- Graph status at review: 483 nodes, 7549 edges, 13 Go files; current SHA and built-at commit both `cb91dc3317ef273f9d006056dc61aacca0c2b3a7`.
- Changed surface: `cmd/herd/fence_ops.go`, its CLI registration/help/control-surface entries and tests, `pkg/provider/fence_broker_client.go` plus lookup tests, and `pkg/claim` outbox/provider-transition additions.
- Graph call paths confirm `runFenceOps` dispatches `runFenceOpStatus` and `runFenceOpReconcile`; both call the new `FenceBrokerClient.LookupOp`. `findOutboxRecord` calls the new public `SQLiteOutbox.FindByPayload`.
- The exact-op checks, broker path validation, report-only default, settle refusal, outbox identity retention, and no-provider-write fixtures passed the focused suite. The two findings above remain blocking for this R3 candidate.

## Tests run

Actual commands and exits from the exact candidate:

```text
go test ./pkg/provider -run '^(TestFenceBrokerClientLookupOpFailClosed|TestValidateOpID)$' -count=1
ok  	github.com/Kampe/Herdforge/pkg/provider	0.265s
exit=0

go test ./pkg/claim -run '^TestSQLiteOutboxFindByPayload$' -count=1
ok  	github.com/Kampe/Herdforge/pkg/claim	0.234s
exit=0

go test ./cmd/herd -run '^TestFence(Op|Reconcile)' -count=1
ok  	github.com/Kampe/Herdforge/cmd/herd	3.776s
exit=0

go test -race -count=1 -timeout=300s -run '^TestFence(Op|Reconcile)' ./cmd/herd
ok  	github.com/Kampe/Herdforge/cmd/herd	4.229s
exit=0

go test -race -count=1 -timeout=300s -run '^(TestFenceBrokerClientLookupOpFailClosed|TestValidateOpID)$' ./pkg/provider
ok  	github.com/Kampe/Herdforge/pkg/provider	1.421s
exit=0

go test -race -count=1 -timeout=300s -run '^TestSQLiteOutboxFindByPayload$' ./pkg/claim
ok  	github.com/Kampe/Herdforge/pkg/claim	1.284s
exit=0

go build ./cmd/herd
exit=0

go vet ./cmd/herd ./pkg/claim ./pkg/provider
exit=0

go run ./scripts/hermeticity/
exit=0

git diff --check ec4c5ce7000d1bc8370ffcbd5029f8f9262eb1ec..cb91dc3317ef273f9d006056dc61aacca0c2b3a7
exit=0

git status --porcelain=v1
(empty)
exit=0
```

The coordinator-supplied retained WSL evidence reports a clean exact `make ci` PASS at this SHA, with raw log SHA-256 `66a14aba23badbe4273dff1cbb9e9554411ac635c7d5c84492edb4f94682a4d6`. I did not claim that coordinator-owned gate as an independently replayed managed/native receipt. No GHA, live provider, board, install, push, merge, or protected-operation action was performed by this reviewer.

## Residual risk

Until both fail-closed reader corrections are made and their null/empty plus body-read-error controls are watched GREEN, a broker response can be misclassified as authenticated applied evidence. No merge or settlement authority is granted.

reviewed_at: 2026-09-09T22:06:25-05:00
