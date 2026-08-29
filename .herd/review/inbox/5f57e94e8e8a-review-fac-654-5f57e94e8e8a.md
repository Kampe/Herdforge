sha: 5f57e94e8e8a5b3c42d4710c962e6cca2693dd9b
branch: herd/fac-654
task: FAC-654
reviewer: assayer
reviewer-family: google
builder-family: openai
verdict: PASS
reviewed-head: 5f57e94e8e8a5b3c42d4710c962e6cca2693dd9b
---
Review of FAC-654:

The commit successfully extracts the duplicate `taskScopedGraphRevision` logic from `cmd/herd/envplan.go` and `cmd/herd/main.go` into a single helper function `taskScopedGraphRevision` in `cmd/herd/envplan.go`. 
Both files belong to `package main`, so the function is correctly accessible, keeping the functionality equivalent while eliminating the duplicate code block.

- `pkg/invariant/...` tests (including `TestNoNewDuplicatedRules`) passed successfully.
- Overall `go build ./...` completed without any syntax or type errors.

Verdict: PASS
