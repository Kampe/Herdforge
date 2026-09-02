sha: a0a0704dde90af4c0d8f684d0fb89f632734a832
branch: herd/fac-734
task: FAC-734
reviewer: google
reviewer-family: google
builder-family: xai
verdict: FAIL
reviewed-head: a0a0704dde90af4c0d8f684d0fb89f632734a832
---
The candidate's changes cause the `pkg/herdr` test suite to fail on `TestLegacyAgentListCannotAuthorizeCandidate` with:
`cleanup_test.go:181: mutation Cleanup must fail closed without FAC-180 fence`

This regression appears to be related to the modifications in `pkg/launch/launch.go` where `DefaultSink()` now relies on `gitroot.ProjectRoot`, likely changing the expected behavior of `Cleanup()` when parsing receipts or identifying paths in test environments.
