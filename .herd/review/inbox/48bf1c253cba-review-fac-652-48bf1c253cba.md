sha: 48bf1c253cbae6288fb6d85850807853dad52247
branch: recovery/fac-652-flash-coordinator-fd
task: FAC-652
reviewer: review-fac-652-48bf1c253cba
reviewer-family: openai
builder-family: open-weight
verdict: FAIL
reviewed-base: 93d9c2ba42034d18938adcb28d584790adc0dd91
reviewed-head: 48bf1c253cbae6288fb6d85850807853dad52247
---
## Findings and risk

Reviewed the exact full range `93d9c2ba42034d18938adcb28d584790adc0dd91..48bf1c253cbae6288fb6d85850807853dad52247` (patch ID `d74dc21ada8858b7c8ba531f2a1847c80409a9db`). This is R3 auth/secrets infrastructure. The candidate correctly validates descriptor number/type, consumes the descriptor on the normal constructor path, refuses mint material in the environment, binds the minter to the live lease identity, and the new end-to-end/refusal tests are non-vacuous: replacing `pkg/provider/claim_stack.go` with its parent blob made the integration test and all five refusal subtests fail.

1. [High] `coordinatorMinterFromInheritedFDIfPresent` returns immediately for `HERD_ROLE=worker`, `builder`, or `reviewer` at `pkg/provider/fence_mint_authority.go:184-191`, before `NewFenceBrokerMinterFromInheritedFD` can close the descriptor and unset `HERD_FENCE_MINT_FD`. A process that reaches this refusal with a valid inherited pipe retains the buffered mint secret and the capability marker after `OpenClaimStack` fails, violating the constructor's consume-once cleanup invariant and leaving the secret available to later code in the failed process. The refusal path needs the same guaranteed descriptor/environment cleanup as every other path.

2. [High] The worker-role deny list omits the actual Herdr task/reviewer marker `HERD_ROLE=agent` (`pkg/herdr/herdr.go:1078-1085`). I independently ran the new end-to-end test with `HERD_ROLE=agent`; it passed and attached the coordinator minter. Thus any task/reviewer process that receives or accidentally retains the inherited FD is accepted as a coordinator client. The ambient role variable is explicitly documented as caller-scrubbable metadata, so it cannot be the authority; the production boundary must ensure agent panes cannot use this minter path and must not rely on the incomplete string list.

3. [High] The patch adds only the client consumer. `GrantMintToChild` has no production caller (the only call sites are its two tests), while `cmd/herd/fence_cmds.go:101-125` starts the standalone broker and still instructs operators to provide `HERD_FENCE_BROKER_MINT_TOKEN`/a file-backed mint setup; it never provisions an inherited mint FD to a coordinator child. Consequently the FAC-652 path is reachable in the in-process test fixture but there is no shipped supervisor/CLI flow that supplies the authenticated FD, so ordinary standalone-broker coordinator approval remains unable to use this fix.

Residual risk: the broker's existing worker-token/error-body protections and the lease-derived `WithMintIdentity` path passed the exercised checks, but the role/FD cleanup issues remain security-sensitive until repaired. Verification digest was not supplied by the packet; the independent command results are recorded below.

## Tests run

- `go test ./pkg/provider -run "TestOpenClaimStack_(CoordinatorMinterFromInheritedFD|MintFDRefusalsFailClosed|ClientWithoutMintFDStillNoMinter)$" -count=1 -v` — exit 0.
- `HERD_ROLE=agent go test ./pkg/provider -run "TestOpenClaimStack_CoordinatorMinterFromInheritedFD$" -count=1 -v` — exit 0; demonstrates the actual agent marker is accepted.
- Parent-blob mutation control: temporarily installed base `pkg/provider/claim_stack.go`, ran `go test ./pkg/provider -run "TestOpenClaimStack_(CoordinatorMinterFromInheritedFD|MintFDRefusalsFailClosed)$" -count=1` — exit 1 (integration test and all five refusal subtests failed); restored candidate blob and verified clean status.
- `go test ./pkg/provider -count=1` — exit 0 (`112.614s`).
- `go test -race ./pkg/provider -run "^TestOpenClaimStack_CoordinatorMinterFromInheritedFD$" -count=1` — exit 0.
- `go test -race ./pkg/provider -run "^Test(InProcessMinter|InheritedMint|GrantMintToChild)" -count=1` — exit 0.
- `go test -race ./pkg/provider -run "^TestOpenClaimStack_MintFDRefusalsFailClosed$" -count=1` — exit 0.
- Initial combined focused race command hit fixture-only `SQLITE_BUSY` during setup — exit 1; isolated reruns above passed and no candidate race was reported.
- `go test ./pkg/sync -run "^TestBoardDoneFenced" -count=1` — exit 0.
- `go test ./cmd/herd -run "^TestFencedBoardDone" -count=1` — exit 0.
- `go build ./...` — exit 0.
- `go vet ./pkg/provider` — exit 0.
- `go vet ./...` — exit 0.
- `go run ./scripts/hermeticity/` — exit 0.
- `git diff --check 93d9c2ba42034d18938adcb28d584790adc0dd91 48bf1c253cbae6288fb6d85850807853dad52247` — exit 0.
- Final `git rev-parse HEAD` matched the reviewed head; `git status --porcelain` was empty.

Reviewed exact head remains `48bf1c253cbae6288fb6d85850807853dad52247`; no source, commit, board, host, or process state was changed.
