# Lane launch provenance

`herd up <lane>` uses the same `startStandingAgent` operation as
`herd standing`. After the exact prepared agent starts, that operation writes
an accepted receipt from the resolved route before reporting success. The
receipt includes the lane task identity, branch, provider/model, derived builder
family, repository and exact tab/pane. A failed start writes no accepted lane
receipt. A receipt-write failure returns an error instead of reporting a usable
raise; as with standing, that failure can occur after the agent has started.
Inspect the exact failed launch before recovery rather than treating the error
as proof that no pane exists.

## Other entry points audited for FAC-623

| Entry point | Provenance boundary |
| --- | --- |
| `standing`, including standing recovery/mender roles | Calls `startStandingAgent`; shares the resolved-route writer with `up`. |
| Task-ref `shot` | `shotDispatch` calls `dispatchTicketDecision`, the native task dispatch path. Task receipts and dispatch generation are separate from standing lane identity; this change does not replace them with lane-name receipts. |
| Prompt-only `shot` | Executes a bounded request and returns output through `execShot`; it does not raise a persistent lane. Outside the lane-raise receipt contract. |
| `forge` ephemeral fallback | Uses `StartPreparedAgent`, then `persistForgeTaskReceipt` for the task-specific handoff. It does not call the standing writer; changing its task receipt and timing is outside this lane-raise fix. |
| Direct review startup | Uses candidate-specific review admission and `StartPreparedAgent`. It is a reviewer, not builder launch evidence. |
| Raw `herdr agent start` | External terminal operation, outside Herdforge's command boundary. It does not acquire the standing writer by implication. Do not assume it supplies a Herdforge accepted lane receipt. |

The task/forge and raw-terminal paths above have not been certified equivalent
to the standing receipt writer by this repair. Their distinct authority and
receipt contracts are preserved; FAC-623 does not backfill existing commits or
change the receipt schema, builder-family join, or worktree freshness policy.

## Regression

`go test -count=1 ./cmd/herd -run '^TestFAC623'` drives `runUpCommand`, the
implementation called by the CLI, with a real temporary Git branch/config and
posture, a sealed router decision, and isolated routing/terminal fixtures.
Replacing its `startStandingAgent` call with the old direct start makes the
resolved-provenance and write-failure assertions fail. The tests never launch a
real agent or mutate a live board.
