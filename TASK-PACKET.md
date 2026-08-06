BUILD FAC-201 — EXECUTE. No menus, no questions. Do not stop until `go build ./... && go test ./...` passes AND you have committed.

Worktree: current directory (Herdr cwd-enforced), branch herd/fac-201. Work ONLY here — never edit files outside it.

Read the full spec yourself (do not wait for it inline) via the configured task provider (kaneo):
  kaneo task get FAC-201 --full

Completion contract (self-gate, FAC-116):
  1. You are already in the task worktree (Herdr cwd-enforced).
  2. Implement per the spec you just read (real code + table tests).
  3. `go build ./... && go test ./...` — ALL green.
  4. Verify yourself: herd verify --build "go build ./..." --test "go test ./..." . (must PASS: real commits + build + tests).
  5. git add -A && git commit -m "<msg containing FAC-201>" (no AI-attribution trailers).
  6. Final message: `BUILD COMPLETE FAC-201` + `git rev-parse HEAD`.

Do NOT push, PR, or merge — the coordinator harvests your branch. Do NOT touch the root checkout.
Role contract: .herd/prompts/worker.md

## Structured Dependencies (authoritative)
Machine-readable only. Markdown prose is display-only and is never eligibility authority.

```herd-deps-v1
{
  "version": 1,
  "task_ref": "FAC-201",
  "task_id": "lyuej0jflf2hvqja7r2vds8f",
  "edges": null,
  "graph_revision": "5c602c147b6f60d01098a2fd5a63285d457abec03c256320f1affc82d1d563f6",
  "provider_revision": "536c63b2de7979d5f3c5e12f28a43f85fa28eff1bb780b08fb4e5665c7244bef",
  "recorded_at": "2026-08-06T13:32:21.842579Z"
}
```
