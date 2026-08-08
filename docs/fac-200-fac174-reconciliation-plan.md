# FAC-200: one-time supervised reconciliation plan for the FAC-174 leftovers

> **DISCHARGED 2026-08-08 — no containers remain to reconcile.** Step 1 of the
> supervised procedure below ("re-confirm existence; if an ID is already gone,
> cross it off") was executed against all 18 IDs. Every one is absent from the
> host, so steps 2–6 have nothing to act on and no removal was performed by this
> plan:
>
> ```
> $ docker ps -aq --no-trunc | sort -u > live
> $ comm -12 <(the 18 baseline IDs, sorted) live
> (no output — none of the 18 are present)
> $ docker ps -aq | wc -l
> 10          # all unrelated, all Up: the chainseer service stack
> ```
>
> The plan is kept for provenance and because
> `containerlifecycle.FAC174LegacyBaseline` still pins these 18 IDs, so
> `herd containers` continues to label them correctly should one ever reappear.
> A container matching `fac174-*` that shows up now is a NEW situation needing
> its own audit, exactly as the closing paragraph already says.
>
> Note this plan describes the pre-receipt world. Since FAC-231, the hermetic
> runner registers a durable receipt immediately after create, so this class of
> unowned leftover is no longer produced — see
> [fac-200-integration-status.md](./fac-200-integration-status.md).

## Baseline

Observed live via `docker ps -a --no-trunc` on 2026-08-04, before
`pkg/containerlifecycle`'s receipt store existed. 18 containers (17
`Exited`, 1 `Created`), all from the FAC-174 hermetic verification work.
None were ever registered with a receipt (the store didn't exist yet),
so `herd containers` correctly reports them as unowned — this document,
plus `containerlifecycle.FAC174LegacyBaseline`, is what turns that
generic "unowned" signal into a scoped, supervised plan for these
specific 18 IDs rather than a standing invitation to prune anything that
looks unowned.

| # | Full container ID | Image | Status (as of 2026-08-04) | Name |
|---|---|---|---|---|
| 1 | `18381a40355cf1333b45c51862b1d3ad16976126a233c8fbcba107571a267ef6` | fac174-hermetic:54ac3e2-h12 | Exited (0) | fac174-hermetic12-test-54ac3e2 |
| 2 | `8fcf80b37d6dcb11925efbbe7cf7e4c6dba2c5f1fc987fc9aca6ca0b4b3f4b5a` | fac174-hermetic:54ac3e2-h12 | Exited (0) | fac174-h12-content-probe-54ac3e2 |
| 3 | `b830c3b4d07facc23f6a472516e90004e258883db5087f75c8a5fa162c8275a9` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic11-test-d0cc889 |
| 4 | `3468570c2d35d61d727332ce023adc9bbf3fb2d3f5bc342500fa89ad99ec81c4` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic10-test-d0cc889 |
| 5 | `784ccb775c84d6b1aeb4fbbddd94e6e387a32bcd91b5c2fd7b0f62cd7399ae2c` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic9-test-d0cc889 |
| 6 | `432dedda3cd03b1c3d5ca52afe87784c426d1f0f47afbcbf6020f09758c187bb` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic8-test-d0cc889 |
| 7 | `92323e57cb7221cc226bc28499985e9a0a4e4707dfaaf63ec786c52d5f320039` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic7-test-d0cc889 |
| 8 | `d8b77814ea9deeaa74f2783462df3896ea52e523d4136faa8f8380a62d92ae77` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic6-test-d0cc889 |
| 9 | `8f8bd32b79c99995f4a6217601fe1895d562ccd3df64bbb73b8f9fd940ac5ca3` | sha256:fdbb19ae3f04... | Exited (101) | fac174-hermetic5-test-d0cc889 |
| 10 | `ad8f9ad68d0667e14367cfac804c3ec39c399aa3d9711407af04a570a3e0a7fb` | fac174-hermetic:d0cc889-zigbuild-closure | Exited (1) | fac174-hermetic4-test-d0cc889 |
| 11 | `6a282ab14e34693c3f871b012bd4eadedb2ce8ada6bf10ca39a8a763ee0472a6` | fac174-hermetic:d0cc889-zigbuild-closure | Created | fac174-hermetic4-imageproof-d0cc889 |
| 12 | `ebad9ea54f615be75d31427647a43b1f3b70c05bd1d3b22a5a6a4a59a9df9361` | fac174-hermetic:d0cc889-zigfetch-options | Exited (0) | fac174-hermetic3-env-d0cc889 |
| 13 | `9ca6e49e0c84fefb9a8f9c9469246166e75b328ac6791c390da6438087d30327` | fac174-hermetic:d0cc889-zigfetch | Exited (101) | fac174-hermetic2-test-d0cc889 |
| 14 | `7b1d9e97cf34b08e14e6ea14d7320282ccde6fb39ae1b812bb3ea82f296935a9` | fac174-hermetic:d0cc889-zigfetch | Exited (0) | fac174-hermetic2-env-d0cc889 |
| 15 | `afffce0037d621772ac544c4f9bde3019e3b33d702f7213e8f9294f48f997c0a` | fac174-hermetic:d0cc889-zigfetch | Exited (101) | fac174-test2-d0cc889 |
| 16 | `f707d26f93879fc61c8648e3518eee52ae94dc75cdc0b6de10dfc654967c0e4e` | fac174-hermetic:d0cc889-zigfetch | Exited (0) | fac174-env2-d0cc889 |
| 17 | `b78ff8cc262c709cf85d478f6ada32b385da86bd4e4743fd0a7b2ba05003e15c` | fac174-hermetic@sha256:b7b6493a... | Exited (101) | fac174-test-d0cc889 |
| 18 | `393cfa606510820f5338733990136b0f906fa5a35d6b98f870a4fdec5de1c81e` | fac174-hermetic@sha256:b7b6493a... | Exited (0) | fac174-env-d0cc889 |

The same 18 full IDs are pinned in code as
`containerlifecycle.FAC174LegacyBaseline`
(`pkg/containerlifecycle/fac174.go`); `herd containers` uses
`LabelFAC174Baseline` to print this exact baseline as its own section,
separate from any other unowned container so a newly-appeared straggler
is never silently lumped in with (or hidden by) this already-reviewed
set.

## Why these aren't auto-removed

FAC-200 requires that cleanup never identify targets by name substring,
glob, `docker prune`, image ancestry, or mutable task status — and these
containers predate the receipt store, so by definition no durable
receipt authorizes touching them. `AuditUnowned` and `Reconcile` both
enforce this structurally: `Reconcile` only ever iterates receipts, so
these IDs are outside its reach entirely, by construction, not by a
runtime check that could regress.

## Supervised procedure (manual, one time, per ID)

Run this after this branch lands, before removing any of the 18:

1. **Re-confirm existence and identity.** `docker inspect <id>` for each
   ID in the table above. If an ID is already gone, cross it off — no
   action needed for it.
2. **Confirm no receipt exists.** `herd containers --json` (or a direct
   `containerlifecycle.Store.Get` lookup) must show no receipt for the
   ID. If one now exists (e.g. a later, unrelated re-use of that exact
   ID string — vanishingly unlikely, but the point of this step is to
   make it impossible to skip), STOP: something has changed since this
   plan was written and it needs re-review, not a rubber-stamped removal.
3. **Confirm FAC-174 is closed.** Check the FAC-174 card status and that
   no in-flight hermetic verification currently depends on these
   containers or their images.
4. **Get independent sign-off.** A second person (or reviewer distinct
   from whoever runs step 5) confirms steps 1–3 for the batch.
5. **Remove one at a time, by exact ID.** `docker rm --force <id>` per
   ID — never a bulk `docker container prune`, never a name/image
   filter. Log each command and its output.
6. **Prove absence.** Re-run `docker ps -a --no-trunc` afterward and
   confirm: every removed ID is gone, and every ID NOT in this batch
   (including any container unrelated to FAC-174) is still present and
   unchanged.

This plan is scoped to exactly the 18 IDs above. A container that shows
up later matching the `fac174-*` naming pattern is a NEW situation, not
covered by this sign-off, and needs its own audit.
