sha: 4d55dbe2719fd566935478cc7bcebbd7f9ced2ab
branch: recovery/fac-793-wsl-capacity
task: FAC-793
reviewer: review-fac-793-4d55dbe2719f
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: 5bb38a3e65bb77a5cadce7cab31be52f9d404803
reviewed-head: 4d55dbe2719fd566935478cc7bcebbd7f9ced2ab
---
## Findings and risk

Reviewed exact range `5bb38a3e65bb77a5cadce7cab31be52f9d404803..4d55dbe2719fd566935478cc7bcebbd7f9ced2ab`. `herd review-classify` independently reports an R3 floor, with deterministic verification, different-family review, security-capable review, and high-risk explicit gates required. The candidate addresses the guest-rich/host-poor incident and its focused RED/GREEN test is non-vacuous, but the following merge-blocking issues remain:

Lineage note: the native recorded primary launch builder-family is normalized `open-weight`, which is preserved in this corrected artifact for intake compatibility. The actual author vendor was Google; the reviewer vendor family is OpenAI, and this remains an independent different-family review. No original artifact or launch-ledger record is rewritten.

1. [High] `pkg/resources/wsl_capacity.go:251-252` interpolates the extracted UNC volume into a single-quoted PowerShell string. `ExtractWindowsDrive` permits apostrophes and other PowerShell syntax in the server/share components, so a registry BasePath such as `\\server\share'; <command>; #\folder` can terminate the literal and execute host commands. The UNC path branch is explicitly in scope, and the tests cover spaces but not adversarial quoting. The probe must make the volume a data value without executable interpolation, or reject/escape the full UNC input before invoking PowerShell.

2. [High] `pkg/resources/wsl_capacity.go:124-131` takes the minimum free bytes and minimum total bytes independently, then `capacityMetrics` derives the admission percentage from that mismatched pair. For example, guest 1 TiB/800 GiB free and backing 2 TiB/500 GiB free becomes 1 TiB/500 GiB (50%) even though the limiting backing volume is at 25%. A percent-only or sufficiently low-byte-reserve policy can therefore admit work below the physical backing-volume threshold. The limiting resource and its free/total pair must be preserved when evaluating percentage headroom.

3. [High] `OSWSLCommandRunner.Run` at `pkg/resources/wsl_capacity.go:25-30` uses `exec.CommandContext`, whose cancellation kills the direct process only; no process-group/job containment or bounded descendant cleanup is installed. A PowerShell probe that creates a child can leave that child running after the deadline while `StatFS` returns. This does not meet the packet’s explicit “no child orphan” requirement and is not covered by the mock timeout test.

4. [Medium] `DiskEvidence` states at `pkg/resources/disk_capacity.go:187-188` that paths are never included, but the new `BackingBasePath` field is copied verbatim at lines 225 and 589. Registry BasePath commonly contains the Windows username and package/storage layout, so denial evidence can expose a local host path in serialized/logged output. Keep provenance opaque or otherwise maintain the path-free evidence contract.

The requested separate guest/backing provenance is otherwise present, non-WSL passthrough and fail-closed interop paths are covered, and no destructive or live WSL probe was run from this macOS reviewer surface. Residual risk remains that the real `OSBackend{}` production seam is not independently exercised on WSL here; the graph reports `OSBackend`, `EvaluateDiskCapacity`, and related production seams as untested. FAC-613c25’s independent process-census/gridlock failure is not reassessed or expanded into this review.

patch_id: e6048ad96b98cd1d7dae44a2527dba4a08f851c1
verification_digest: sha256:38cd101227f935145ce8da5cd42dc65defaa43383be6babf1befbd7c8616f14d
full_range: 5bb38a3e65bb77a5cadce7cab31be52f9d404803..4d55dbe2719fd566935478cc7bcebbd7f9ced2ab

## Tests run

- `code-review-graph status --repo . --json`: exit 0; 803 nodes, 11025 edges, 27 files, indexed at reviewed head.
- `code-review-graph detect-changes --repo . --base 5bb38a3e65bb77a5cadce7cab31be52f9d404803 --brief`: exit 0; 4 changed files, 41 changed functions, 30 test gaps, overall graph risk 0.60.
- `herd review-classify recovery/fac-793-wsl-capacity --tier R3 --pin 4d55dbe2719fd566935478cc7bcebbd7f9ced2ab --json`: exit 0; inferred/effective/explicit floor R3.
- `go test -v ./pkg/resources -run 'Test(WSL|Parse|Extract)'`: exit 0; all selected WSL/parsing/extraction tests passed.
- `go test -race ./pkg/resources`: exit 0; package passed.
- `go vet ./pkg/resources/...`: exit 0; clean.
- `go build ./cmd/herd`: exit 0; clean.
- `go run ./scripts/hermeticity/`: exit 0; clean.
- `git diff --check 5bb38a3e65bb77a5cadce7cab31be52f9d404803 4d55dbe2719fd566935478cc7bcebbd7f9ced2ab`: exit 0; clean.
- RED mutation: replaced `effectiveFree := minUint64(guestCap.FreeBytes, info.BackingFreeBytes)` with guest-only capacity and ran `go test -v ./pkg/resources -run TestWSLBackend_GuestRichHostPoorIncidentRecreation`: exit 1, assertion reported `Effective FreeBytes = 827855667200, want 16106127360 (bounded by host)`.
- Restored the tracked file with `git checkout -- pkg/resources/wsl_capacity.go`; exit 0, HEAD remained `4d55dbe2719fd566935478cc7bcebbd7f9ced2ab`, and `git status --porcelain` was empty.
- GREEN restoration: `go test -v ./pkg/resources -run TestWSL`: exit 0; all 7 WSL tests passed.

## Author instructions

Return this exact candidate for repair. Add regression coverage for UNC quoting and mismatched guest/backing totals, implement bounded descendant cleanup, and preserve path-free evidence. Re-run the R3 gates on a new exact candidate SHA; this verdict grants no merge authority.

reviewed_at: 2026-09-10T04:23:48Z
