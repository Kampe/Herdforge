sha: a40a7c4473cf874571d5435028dc73d1fc3accb6
branch: recovery/wsl-physical-capacity-1506
task: FAC-792
reviewer: review-fac-792-a40a7c4473cf
reviewer-family: anthropic
builder-family: google
verdict: PASS
reviewed-base: 91f2e712b72266c6246a84a21e58be6f48f9b7bf
reviewed-head: a40a7c4473cf874571d5435028dc73d1fc3accb6
---
## Findings and risk

**Reviewed range**: `91f2e712b72266c6246a84a21e58be6f48f9b7bf..a40a7c4473cf874571d5435028dc73d1fc3accb6`
(full range, 4 commits: 82655402, 141de426, 8fba2a89, a40a7c44 — not started partway through).
patch_id: `8233904bc67100f38dcc297150dff858e35b9818` (independently recomputed via
`git diff <base>..<head> | git patch-id --stable` — matches the packet value).
verification_digest: `sha256:5e1aec53d99512fdcb83fd3717829395ab79613260d18ceccc3ae557201b165a`
full_range: `91f2e712b72266c6246a84a21e58be6f48f9b7bf..a40a7c4473cf874571d5435028dc73d1fc3accb6`

The verification_digest above is quoted from the packet and identifies coordinator-retained
evidence at `.worktrees/orchestrator/.herd/coordinator-resume/completion-recovery-20260909/fac792-review-evidence-1548.json`.
I did not open that file (it is outside my leased surface) and it is not a managed receipt, so I
neither confirm nor rely on it. Every result below is from commands I ran myself inside the pool.

**Risk tier**: classifier floor is **R2**, not lowered.
`herd review-classify recovery/wsl-physical-capacity-1506 --pin a40a7c44 --json` →
`inferred=R2 effective=R2 explicit_floor=none`, rules `r1.tests` (R1, `disk_wsl_test.go`) and
`r2.production_source` (R2, `disk_wsl.go`, `statfs_unix.go`, `statfs_other.go`), required gates
`deterministic_verification`, `different_family_review`, `integration_rerun`. The packet declares
urgent **R3**; I reviewed at R3 (the higher of the two) and assessed the R3 gates explicitly below.
The R2/R3 disagreement is recorded, not resolved by me.

**Independence**: reviewer-family `anthropic` (Claude Opus 5) vs packet-proven builder-family
`google`. Families differ, so the R1–R3 different-family gate is satisfied. builder-family was
taken verbatim from the launch record and not re-derived. It does not read "unproven", so no
hand admission is required on that ground.

**Isolation**: `git rev-parse --show-toplevel` =
`.herd/pool-fac679-haiku-2331/pool-01` (my exclusive leased slot, not the canonical shared
checkout). Every git command, build, test and mutation below ran inside that tree. `git rev-parse HEAD`
= `a40a7c4473cf874571d5435028dc73d1fc3accb6`, matching `sha:`.

**Identity verified independently** (all four claims in the retained source report check out):
- `pkg/resources/disk_wsl.go` sha256 `dfea8da682ae306993dc94371c3e7f846e2136d57c861d00821dcdc4fd7add25`
- `pkg/resources/disk_wsl_test.go` sha256 `49b1bbcdd38823d7473e643a495b6634958b6d88e3166adab187ac062171282f`
- `pkg/resources/statfs_unix.go` sha256 `5e41d2f7ae58c140f2bb10efbb4574ac2ee50ed7552ab087f16ddaf39d3c3c31`
- `pkg/resources/statfs_other.go` sha256 `98672a4abd02439253812cfacfafe9a7a7775eda631b8ea63b934c3aa863194d`
- candidate tree sha `c01a563d48c8f15f7646c3dde6fbfdacd6f012f4`
- `git diff --stat 8fba2a89..a40a7c44` = `pkg/resources/disk_wsl_test.go | 100 ++++---`, one file,
  89 insertions / 11 deletions. **a40a7c44 is strictly test-only**; production source is byte-identical
  to candidate 8fba2a89, which the packet records as independently assessed sound. Confirmed by SHA,
  not by trusting the claim.
- branch binding: `refs/heads/recovery/wsl-physical-capacity-1506` → `a40a7c44` exactly. The pool
  worktree is checked out detached at the pinned SHA (`git branch --show-current` is empty), which is
  correct for an immutable-pin review.

### Independent assessment of the acceptance criteria

**1. Host-volume authority instead of guest virtual free space — MET.**
`boundWSLCapacity` (`pkg/resources/disk_wsl.go:466`) caps `FreeBytes` to `min(guestFree, hostFree)`
and `TotalBytes` to `min(guestTotal, hostTotal)` from the physically probed Windows volume, and it
reaches that volume through registry-authenticated distro → drive-letter → mount-point resolution
rather than trusting the guest ext4 numbers. The WSL2 rationale in the doc comment is correct: a guest
ext4 filesystem on a sparse VHD reports its virtual size, so uncapped guest free space is an
over-report of physically writable bytes.
The capping is monotone-safe: `min(gF,hF) <= gF <= gT` and `min(gF,hF) <= hF <= hT` (the latter
because `probeHostVolumeCapacity` rejects `freeBlocks > totalBlocks`), therefore
`min(gF,hF) <= min(gT,hT)` and capping can never manufacture `FreeBytes > TotalBytes` from a valid
guest reading. I checked this because `validCapacity` (`disk_capacity.go:638`) hard-blocks on that
inversion and a capping bug there would have converted an over-report into a permanent outage.

**2. Bounded subprocess calls with no unbounded fallback — MET in the code.**
`probeHostVolumeCapacity` uses `exec.CommandContext(ctx, ...)` under the 3s
`wslProbeTimeout` context created in `boundWSLCapacity`, and `wslRegistryQueryExecutor` is likewise
`exec.CommandContext`. There is no in-process `unix.Statfs` on a 9p/drvfs path anywhere in the new
code — I grepped for it specifically, because an in-process statfs on 9p is the D-state hang this
whole design exists to avoid, and the only `unix.Statfs` call is `statFSUnix` on the guest path.
`stat -f -c "%S %b %a %c %d"` is the correct GNU format pairing: `%S` is the fundamental block size
for block counts and `%b`/`%a` are counts in those units, so `%S * %b` is genuinely total bytes
(`%s` would have been the wrong multiplier). No test proves the boundedness, however — see F2.

**3. Invalid zero / overflow / free>total rejection — MET.**
`blockSize == 0 || totalBlocks == 0` → error; `freeBlocks > totalBlocks` → error; both byte products
go through `checkedMul`, whose `c/a != b` test is the correct uint64 overflow predicate. Parse errors
on any of the five fields and a short field count both fail closed. `boundWSLCapacity` additionally
rejects `hostCap.TotalBytes == 0`.

**4. Windows drive mount identity, contradictory metadata, escaped paths — MET.**
`findDriveMountPath` derives the drive only from the device field or an explicit `path=` option token,
rejects a line whose device and `path=` option disagree, has no mountpoint-name fallback, and decodes
procfs 3-digit octal escapes so `C:\134` and mountpoints containing spaces resolve correctly. The
specific case the packet told me to recheck — an authentic 9p `D:\134` mounted at `/mnt/c` with
`aname=drvfs` and no `path=` option — is correctly rejected for `C:` and accepted for `D:`; I verified
this both by reading the matching logic and by killing a mutant that adds the mountpoint fallback.

**5. Fail-closed propagation end to end — MET.**
Every failure in `boundWSLCapacity` returns `Capacity{}` plus an error, never a partially-bounded or
guest-passthrough value. `OSBackend.StatFS` (`statfs_unix.go:19`) returns that error unchanged, and
`EvaluateDiskCapacity` (`disk_capacity.go:545`) maps a `StatFS` error to
`diskBlocked(e, DiskReasonUnavailable)`. So an unresolvable WSL host volume blocks admission rather
than allowing it. I traced this to the end rather than stopping at the package boundary because a
fail-closed guard that a caller converts into an allow is worse than no guard.

**6. Non-WSL platforms unaffected — MET.**
`isWSLEnvironment` returns false when `wslRuntimeGOOS != "linux"`, so darwin and non-WSL linux take
the unchanged guest-passthrough path. Blast radius of the changed behavior is therefore WSL-only,
reaching `cmd/herd/resource_governor.go:44`, `pkg/resources/governor.go:331`,
`pkg/worktree/worktree.go:59` and `pkg/harvest/harvest.go:118` — i.e. worktree admission and harvest.

### Test non-vacuity: independent mutation testing

The packet's core question is whether a40a7c44 actually closed the surviving-mutant gap. I did not
take the retained report's RED transcripts on trust; I re-derived them, and then invented eleven more
mutants of my own to look for gaps the report did not claim to cover. Each mutant was applied inside
the pool, run, and reverted from a pristine copy taken before any edit.

**Both commissioned mutants are genuinely killed, with output matching the report exactly:**

- **M1 — `checkedMul` for total bytes replaced by raw `blockSize * totalBlocks`**: killed.
  `TestWSLProbeHostVolumeMultiplicationOverflowFailsClosed` (`disk_wsl_test.go:451`) and
  `TestWSLDirectProbeHostVolumeMultiplicationOverflow` (`disk_wsl_test.go:465`) both failed with the
  same assertion text the report quoted. The `createFakeStatBinary` helper is the reason this works:
  it puts a real `stat` on `PATH` and lets the default `wslDriveStatFS` seam run the actual production
  `probeHostVolumeCapacity`, so the test now exercises production parsing and arithmetic instead of a
  stubbed error string. That is a real repair of the previous vacuity, not a re-worded assertion.
- **M3 — mountpoint-only `/mnt/<letter>` fallback added to `findDriveMountPath`**: killed, by all
  three tests the report named (`:264`, `:276`, `:315`), with matching text.

**Nine further mutants I introduced, all killed** (these were not claimed by the report; I added them
to test whether the rest of the range is non-vacuous too):
M4 contradictory device-vs-options rejection removed → `:297`. M5 `freeBlocks > totalBlocks` guard
removed → `:485`. M6 zero blockSize/totalBlocks guard removed → `:495`. M7 `FreeBytes` host cap
removed → `:627`, `:904`, `:959`. M8 `TotalBytes` host cap removed → `:630`. M9 probe error swallowed
into `return guestCap, nil` → `:451`, `:538`. M10 multi-distro ambiguity resolved to the first distro
instead of failing closed → `:102`, `:158`. M14 drvfs-path bypass removed → `:718`.
The M7 kill is the load-bearing one: it fails `TestOSBackendWSLPhysicalBoundProductionRegressionRED`
through the real `OSBackend.StatFS` entry point, so the physical bound is proven at the production
seam and not only at the internal helper.

**Three mutants survived.** All three are on code that is correct by inspection; none is a defect in
the candidate, and I confirmed each survival is a coverage gap rather than a behavior bug:

1. **F1 (low) — the free-bytes overflow guard is unreachable, and the test that claims to cover it
   does not.** Deleting `checkedMul`+guard for `freeBytes` entirely leaves the whole targeted suite
   GREEN. This is not fixable by a better test: `freeBlocks > totalBlocks` is rejected earlier, so if
   `blockSize * totalBlocks` did not overflow then `blockSize * freeBlocks` cannot either. The branch
   is dead defensive code. Relatedly, Case 2 of `TestWSLDirectProbeHostVolumeMultiplicationOverflow`
   is mislabelled: with inputs `10000000000000000000 3 2 ...` **both** products overflow uint64
   (I checked: 1e19*3 and 1e19*2 both exceed 2^64-1), the total-bytes guard fires first, and the
   assertion is written permissively (`!contains "total bytes overflow" && !contains "free bytes
   overflow"`) so it passes on the total-bytes error. Case 2 therefore duplicates Case 1.
   *Correction*: either drop the free-bytes guard and its case as unreachable, or tighten the Case 2
   assertion to require `"free bytes overflow"` — at which point it will fail and expose the
   unreachability. Do not leave a case whose name asserts coverage it cannot have.
2. **F2 (medium, highest-value follow-up) — subprocess boundedness is untested.** Rewriting
   `exec.CommandContext(ctx, statBin, ...)` to `exec.Command(statBin, ...)` — removing the bound
   named by acceptance criterion 2 — leaves the entire targeted suite GREEN. Removing the
   `if ctx.Err() != nil` timeout classification branch likewise leaves it GREEN.
   `TestWSLHostStatFSCancelledProbeFailsClosed` does not cover this: it stubs `wslDriveStatFS` to
   return a hand-written `"host volume probe timed out"` string and then asserts on that same string,
   so it proves only that `boundWSLCapacity` propagates an error — which M9 already proves. The
   production timeout path is unexercised.
   *Correction*: point `createFakeStatBinary` at a script that sleeps past `wslProbeTimeout`, call
   `probeHostVolumeCapacity` with a short-deadline context, and assert both a non-nil error containing
   `"timed out"` and that the call returns near the deadline rather than after the sleep. Watch it RED
   against the `exec.Command` mutant before landing it.
3. **F3 (low) — `hostCap.TotalBytes == 0` guard uncovered.** Removing it leaves the suite GREEN. It is
   unreachable through the real probe (which rejects zero block size and zero total blocks) and is
   reachable only through an injected `wslDriveStatFS` stub, so it is seam-level defensive depth.
   *Correction*: optional; a one-line stub-returns-zero test would close it.

### Further findings

4. **F4 (medium, non-code) — the packet's pasted card body does not describe this candidate.** The
   `Card:` section of the review packet is entirely about `pkg/herdr` OpenCode exact prompt
   delivery/consumption (Outcome "Native OpenCode review launch proves that its exact packet was
   consumed", Incident at 2026-09-10T02:11Z about `pkg/herdr.Send` timing out, Scope naming
   `pkg/herdr` adapters and `cmd/herd review-launch`, acceptance criteria about staged/stale/
   wrong-session submission and `OpenUsage`, dedupe referencing FAC786/FAC790/FAC791/FAC650). The
   candidate touches only `pkg/resources` WSL disk capacity and nothing in `pkg/herdr` or
   `cmd/herd`. The packet's own leading instruction paragraph, the branch name, the retained source
   report and the diff all agree with each other on WSL physical capacity; only the pasted card body
   disagrees. Reviewer protocol step 1 requires the diff and acceptance criteria to match the assigned
   task, so I am flagging this rather than silently reviewing against whichever text I preferred: I
   assessed the WSL acceptance criteria stated in the packet's instruction paragraph, which is what
   the code implements. I kept `task: FAC-792` because that is the binding the packet and launch
   record assign, and I did not touch the board.
   *Consequence the supervisor must resolve before any closure*: if FAC-792's real card is the
   OpenCode delivery task, this PASS says nothing about it and closing FAC-792 on this verdict would
   record a done card that lies. If the pasted body is a packet-assembly artifact and FAC-792 is the
   WSL recovery card, no action is needed beyond correcting the packet. I cannot tell which from
   inside my surface — `grep -rl FAC-792 .herd/` in the candidate surface returns nothing.
5. **F5 (low) — guest capacity is capped without validating the guest invariant.** The host reading is
   checked for `freeBlocks <= totalBlocks`, but `boundWSLCapacity` does not apply the same check to
   the incoming `guestCap`, and `capacityFromStatfs` (`disk_capacity.go:651`) only guards overflow,
   not `available <= blocks`. A pathological guest reading of `FreeBytes > TotalBytes` — invalid, and
   normally caught downstream by `validCapacity` — can be laundered into a valid-looking capacity by
   capping (e.g. guest 100 free / 10 total with host 5 free / 1000 total yields 5 free / 10 total,
   which passes `validCapacity`), masking a broken guest statfs instead of blocking on it. Requires
   `Bavail > Blocks` from the kernel, so this is theoretical.
   *Correction*: call `validCapacity(guestCap)` on entry, or apply the same `free <= total` guard.
6. **F6 (low, performance) — no caching; the probe runs per `StatFS` call.** Inside WSL every
   `StatFS` spawns a `reg.exe` query plus a `stat` subprocess, each with a 3s bound.
   `EvaluateDiskCapacity` calls `StatFS` once for `request.Path`, again for `TempPath`, and once per
   `AdditionalPaths` entry (`disk_capacity.go:545`, `:568`, `:589`), so a root+temp+2-additional
   admission check is up to 8 subprocess spawns and a 24s worst-case tail on a slow 9p mount, on
   every worktree-creation and harvest admission. Correctness is unaffected and non-WSL hosts pay
   nothing.
   *Correction*: memoize the resolved drive letter and mount path (they cannot change within a
   process) and optionally the host capacity for a short TTL.
7. **F7 (informational) — `TestWSLProbeHostVolumeMultiplicationOverflowFailsClosed` depends on
   `wslDriveStatFS` being globally pristine.** After a40a7c44 it deliberately no longer saves and
   restores that seam, because it needs the default. Every other test that assigns the seam does
   restore it in a `defer`, so this is sound today, and M1 proves the test is non-vacuous under the
   current ordering. But a future test that leaks an override would silently make this one vacuous
   rather than fail it. `-shuffle=on -count=2` passed, so there is no order dependence right now.

### Residual risk

- **Not verified: real WSL.** Everything above ran on darwin/arm64 with hermetic seams and a fake
  `stat` on `PATH`. No real `/proc/mounts`, no real `reg.exe`, no real 9p/drvfs volume, no Windows or
  PowerShell was exercised. Per the packet, root retains the actual WSL host probe and the final
  Linux CI run; this verdict does not substitute for either, and F2 in particular is a gap that only a
  real timing test or the WSL run can close.
- **Context cancellation cannot guarantee a hard kill.** `exec.CommandContext` sends a signal and
  `cmd.Run` returns, but a `stat` blocked in an OS-uninterruptible D-state wait on a wedged 9p mount
  will not die on signal. The 3s bound bounds *the caller*, not the child. A hung 9p volume can
  therefore still leak a zombie-until-reaped `stat` child even though the Herdforge call returns
  fail-closed on time. This is a genuine platform limit, not a defect in the candidate, and it is the
  reason the design correctly avoids in-process `unix.Statfs` on those paths.
- **Availability change on WSL hosts.** Fail-closed is the right default, but it converts several
  previously-tolerated conditions into a hard block on worktree creation and harvest inside WSL:
  multiple registered distros with `WSL_DISTRO_NAME` unset, an unreadable Lxss registry, no `reg.exe`
  reachable, or no `stat` on `PATH`. This fleet has a live WSL review host, so the first real WSL run
  should confirm the happy path resolves before this is relied on for admission — a WSL box that fails
  any of those probes will go dark for disk-gated operations rather than degrade.
- **Mount authority rests on the `/proc/mounts` device and `path=` fields.** Any `9p` line is treated
  as candidate DrvFS, so a mount whose device field claims `C:` is believed. Mounting requires root in
  WSL, so this is an accepted trust boundary rather than a new hole, but it is worth naming: the guard
  defends against accidental mountpoint-name collisions and contradictory metadata, not against a
  root-level forged mount.
- **Scope not covered by me, by instruction**: no full Mac CI, no full-repo test suite, no real
  Windows/PowerShell/provider/credential exercise, no cleanup mutation, no process stopped outside my
  own fixture children, no commit/push/merge/board write, no live resource cleanup. Package-level
  checks below do not establish full-suite coverage. The retained builder/CI receipts are separate
  evidence for candidate a40a7c44 and are not represented as my execution.

### Verdict rationale

PASS. a40a7c44 is strictly test-only on production source byte-identical to the previously
independently-assessed-sound 8fba2a89, so it cannot regress behavior, and I confirmed that by SHA
rather than by claim. Its stated purpose was to make the `checkedMul` overflow guard and the mount
authority guard non-vacuous; I independently reproduced both kills with output matching the retained
report, and the `createFakeStatBinary` change is a real repair — the overflow test now runs the actual
production probe instead of asserting on a string it wrote itself. Nine additional mutants of my own
across the full range were also killed, including one that fails through the real `OSBackend.StatFS`
entry point, so the physical bound is proven at the production seam. The full-range implementation is
fail-closed end to end into `EvaluateDiskCapacity`, the capping is monotone-safe, and non-WSL hosts
are untouched. The three surviving mutants are all on code correct by inspection: two guard provably
unreachable states (F1, F3) and one is a one-line stdlib boundedness idiom whose absence of a test is
a coverage gap, not a defect (F2). No blocking finding remains for this exact revision.

F2 and F4 are the two the supervisor should act on. F2 wants a follow-up card — a real
sleep-past-deadline test watched RED against the `exec.Command` mutant — since an untested bound is
how criterion 2 silently regresses later. F4 must be resolved before FAC-792 is closed: the card body
in the packet describes a different work item, and this verdict speaks only to the WSL physical
capacity range. No card closure is inferred from this verdict.

## Tests run

All commands executed by me, inside the leased pool worktree
`.herd/pool-fac679-haiku-2331/pool-01`, at HEAD `a40a7c4473cf874571d5435028dc73d1fc3accb6`.
`unset GOROOT` prefixed every Go command (this workspace has a stale `GOROOT` that otherwise breaks
the toolchain). Exit statuses are the direct status of the command shown.

1. Isolation and identity
```
git rev-parse --show-toplevel   -> /Users/kampe/Personal/Herdforge/.herd/pool-fac679-haiku-2331/pool-01
git rev-parse HEAD              -> a40a7c4473cf874571d5435028dc73d1fc3accb6
git status --porcelain          -> (empty)
git log --oneline 91f2e712..a40a7c44 -> a40a7c44, 8fba2a89, 141de426, 82655402  (4 commits)
git diff --stat 8fba2a89..a40a7c44   -> pkg/resources/disk_wsl_test.go | 100 +++---  (1 file, +89 -11)
git rev-parse a40a7c44^{tree}   -> c01a563d48c8f15f7646c3dde6fbfdacd6f012f4
git diff 91f2e712..a40a7c44 | git patch-id --stable -> 8233904bc67100f38dcc297150dff858e35b9818
git for-each-ref | grep wsl-physical -> refs/heads/recovery/wsl-physical-capacity-1506 a40a7c44
shasum -a 256 pkg/resources/{disk_wsl.go,disk_wsl_test.go,statfs_unix.go,statfs_other.go}
  -> dfea8da6..., 49b1bbcd..., 5e41d2f7..., 98672a4a...  (all four match the retained report)
```
All exit 0. Every identity claim in the retained source report independently confirmed.

2. Risk classification
```
herd review-classify recovery/wsl-physical-capacity-1506 --pin a40a7c4473cf... --json   (exit 0)
  -> target=a40a7c44 base=origin/main paths=4 insertions=1504 deletions=0
  -> inferred=R2 effective=R2 explicit_floor=none
  -> evidence=r1.tests tier=R1 ; evidence=r2.production_source tier=R2
  -> required_gates: deterministic_verification, different_family_review, integration_rerun
```
Floor R2, not lowered. Packet declares R3; reviewed at R3.

3. Build and vet
```
go build ./...                                              -> exit 0, no output
go vet ./pkg/resources                                      -> exit 0, no output
go vet ./cmd/herd ./pkg/resources ./pkg/worktree ./pkg/harvest -> exit 0, no output
```

4. Baseline GREEN — targeted, with race
```
GOMAXPROCS=2 go test -race -p 2 -count=1 ./pkg/resources -run "TestWSL|TestExtract|TestOSBackend"
  -> ok github.com/Kampe/Herdforge/pkg/resources 3.999s   (exit 0)
```

5. Baseline GREEN — whole changed package, with race
```
GOMAXPROCS=2 go test -race -p 2 -count=1 ./pkg/resources
  -> ok github.com/Kampe/Herdforge/pkg/resources 22.625s  (exit 0)
```
Package-level only. This is not full-suite coverage.

6. Order-dependence / global-seam check
```
GOMAXPROCS=2 go test -race -p 2 -count=2 -shuffle=on ./pkg/resources -run "TestWSL|TestExtract|TestOSBackend"
  -> ok github.com/Kampe/Herdforge/pkg/resources 6.628s   (exit 0)
```
No order dependence despite the package-level mutable seams.

7. Mutation testing — production-source regressions, each compiled, run, then reverted
Method: `pkg/resources/disk_wsl.go` copied to the session scratchpad before any edit; each mutant
applied to the pool copy, `go test -p 2 -count=1 ./pkg/resources -run "TestWSL|TestExtract|TestOSBackend"`
run, then the pristine copy restored. Restoration verified byte-identical by sha256 after each mutant.
Only `pkg/resources/disk_wsl.go` inside the pool was ever modified; no test file was touched.

KILLED (mutant detected — RED with a direct assertion failure, `go test` exit 1):
```
M1  checkedMul -> raw blockSize*totalBlocks
    FAIL TestWSLProbeHostVolumeMultiplicationOverflowFailsClosed
      disk_wsl_test.go:451: expected overflow probe error to fail closed
    FAIL TestWSLDirectProbeHostVolumeMultiplicationOverflow
      disk_wsl_test.go:465: expected probeHostVolumeCapacity to fail on total bytes multiplication overflow
    (matches the retained report's claimed RED output exactly)

M3  mountpoint-only /mnt/<letter> fallback added to findDriveMountPath
    FAIL TestWSLFindDriveMountPathRejectsMountSpoof
      disk_wsl_test.go:264: expected authentic 9p mount /media/c, got spoofed "/mnt/c"
    FAIL TestWSLFindDriveMountPathRejectsAmbiguous9pDeviceDMountedAtMountCWithoutPathOption
      disk_wsl_test.go:276: expected 9p mount at /mnt/c backed by device D: (without path option) to be rejected for drive C:
    FAIL TestWSLFindDriveMountPathRejectsDeviceDMountedAtMountC
      disk_wsl_test.go:315: expected /mnt/c backed by D: to be rejected when resolving drive C:
    (matches the retained report's claimed RED output exactly)

M4  contradictory device-vs-options rejection removed
    FAIL TestWSLFindDriveMountPathRejectsContradictoryDeviceAndOptions
      disk_wsl_test.go:297: expected contradictory device vs options mount to be rejected for C:

M5  freeBlocks > totalBlocks guard removed
    FAIL TestWSLDirectProbeHostVolumeMultiplicationOverflow
      disk_wsl_test.go:485: expected probeHostVolumeCapacity to fail when freeBlocks > totalBlocks

M6  zero blockSize/totalBlocks guard removed
    FAIL TestWSLDirectProbeHostVolumeMultiplicationOverflow
      disk_wsl_test.go:495: expected probeHostVolumeCapacity to fail when blockSize == 0

M7  FreeBytes host cap removed
    FAIL TestWSLHostVolumeCappingGuestFreeGreaterThanHostFree
      disk_wsl_test.go:627: expected FreeBytes capped to host free bytes 14308425728, got 776875823104
    FAIL TestWSLProductionRegressionPhysicalBoundIgnoredMustBeRED
      disk_wsl_test.go:904: expected physical host bound to BLOCK operation; got allowed with free bytes 776875823104
    FAIL TestOSBackendWSLPhysicalBoundProductionRegressionRED
      disk_wsl_test.go:959: REGRESSION DETECTED: OSBackend.StatFS returned uncapped free bytes 27228672000 > host free bytes 14308425728

M8  TotalBytes host cap removed
    FAIL TestWSLHostVolumeCappingGuestFreeGreaterThanHostFree
      disk_wsl_test.go:630: expected TotalBytes capped to host total bytes 1000000000000, got 1073741824000

M9  probe error swallowed (return guestCap, nil)
    FAIL TestWSLProbeHostVolumeMultiplicationOverflowFailsClosed
      disk_wsl_test.go:451: expected overflow probe error to fail closed
    FAIL TestWSLHostStatFSCancelledProbeFailsClosed
      disk_wsl_test.go:538: expected cancelled probe to fail closed

M10 multi-distro ambiguity resolved to unique[0] instead of failing closed
    FAIL TestWSLResolveDistroBackingDriveHermetic
      disk_wsl_test.go:102: expected error when WSL_DISTRO_NAME is unset and multiple distros are registered
    FAIL TestWSLResolveDistroNoNameMultiDistroNondeterminismFailsClosed
      disk_wsl_test.go:158: expected unauthenticated multi-distro resolution to fail closed

M14 drvfs-path bypass removed (always cap)
    FAIL TestWSLDrvFSMountPathBypassesVHDHostCapping
      disk_wsl_test.go:718: unexpected error: wsl host backing volume detection: query lxss registry: exec: "reg.exe": executable file not found in $PATH
```

SURVIVED (mutant undetected — `go test` exit 0, `ok github.com/Kampe/Herdforge/pkg/resources`):
```
M2  freeBytes checkedMul + overflow guard removed entirely   -> ok (exit 0)   [finding F1]
M11 exec.CommandContext(ctx, ...) -> exec.Command(...)       -> ok (exit 0)   [finding F2]
M12 "if ctx.Err() != nil { timed out }" branch removed       -> ok (exit 0)   [finding F2]
M13 hostCap.TotalBytes == 0 guard removed                    -> ok (exit 0)   [finding F3]
```

8. Reachability check supporting F1
```
python3: bs=10000000000000000000 tb=3 fb=2 ; 2**64-1 = 18446744073709551615
  -> case2 total overflow? True   free overflow? True
```
Both products overflow, the total-bytes guard fires first, so Case 2 of
`TestWSLDirectProbeHostVolumeMultiplicationOverflow` never reaches the free-bytes branch. Combined
with the `freeBlocks <= totalBlocks` precondition this makes the free-bytes overflow branch
unreachable, which is why M2 cannot be killed by any test.

9. Post-mutation restoration verified
```
git status --porcelain                  -> (empty)
git rev-parse HEAD                      -> a40a7c4473cf874571d5435028dc73d1fc3accb6
git rev-parse --show-toplevel           -> .herd/pool-fac679-haiku-2331/pool-01
shasum -a 256 pkg/resources/disk_wsl.go -> dfea8da682ae306993dc94371c3e7f846e2136d57c861d00821dcdc4fd7add25
```
Byte-identical to the candidate blob and to the retained report's recorded sha256. The pool worktree
is clean; no file was left mutated, no commit, push, merge, board write, or worktree/ref mutation was
performed, and nothing outside the leased slot was written except this verdict artifact.

Limitations of the above: darwin/arm64 only; hermetic seams and a fake `stat` binary on `PATH`, not a
real WSL kernel, `/proc/mounts`, `reg.exe`, or 9p/drvfs volume; `./pkg/resources` package scope, which
is not full-suite coverage; the subprocess timeout path (F2) is not covered by any test I could run
here. No full Mac CI, no Windows/PowerShell, no provider/credential/board/live-cleanup action.
