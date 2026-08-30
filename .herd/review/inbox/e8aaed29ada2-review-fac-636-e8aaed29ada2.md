sha: e8aaed29ada2dacda7328e78c9e77df38af63055
branch: fac-636-kaneo-cred-path
task: FAC-636
reviewer: assayer
reviewer-family: xai
builder-family: openai
verdict: PASS
reviewed-head: e8aaed29ada2dacda7328e78c9e77df38af63055
---

No findings.

## Scope

Single-commit candidate `e8aaed29ada2` on `fac-636-kaneo-cred-path` (`fix(provider): prefer non-TCC Kaneo credential path`). Parent/merge-base with `origin/main` is `f2ca0d320043cc57d0262a0c003e898a120e4325`. Diff is two files:

- `pkg/provider/kaneo.go`
- `pkg/provider/kaneo_credentials_test.go`

`docs/prompts/review-contract.md` is absent on this candidate and on `origin/main`. Review followed `.herd/prompts/reviewer.md` plus the packet.

Pool worktree HEAD at review start was `1f4a03038` (FAC-644), not the pin. Isolation was valid: `git rev-parse --show-toplevel` resolved under `.herd/pool/` (`pool-fanout-5/pool-01`); surface symlink points at that exclusive slot. Candidate objects were read via `git show`/`git grep` on SHA `e8aaed29ada2`. Credential tests were executed by overlaying the two candidate blobs, then the tree was restored (`git status --porcelain` clean; files match HEAD).

Risk: R3 (credential lookup). Cross-family gate holds: builder openai, reviewer xai.

## Behavior

`kaneoCLIConfigPath` now tries `$HOME/.config/kaneo/config.json` when `UserHomeDir` returns an absolute home and `os.Stat` succeeds, then falls back to `os.UserConfigDir` via `kaneoConfigPathFromRoot`. Empty/relative config roots still fail closed. Non-`IsNotExist` Stat errors fail closed rather than falling through. Origin-binding, env-key override, and `ResolveKaneoProfileCred` parse rules are unchanged.

`withUserConfigDir` now also stubs `userHomeDirFn` to a temp home so existing UserConfigDir tests cannot accidentally pick up a real `~/.config/kaneo/config.json` after the new preference.

Callers of `ResolveKaneoProfileCred` / `kaneoCLIConfigPath` on the candidate SHA: `kaneo.go` (profile load, API key, authorize), `factory_config.go` (profile-only provider construction), `kaneo_labels.go` (label authority origin). No new trust anchor; repository APIURL is still not used as credential authority.

## Tests

`go test ./pkg/provider -count=1 -timeout 120s -run 'TestResolveKaneo|TestKaneoCLIConfigPath|TestAuthorizeKaneo|TestNewFromHerdConfig'` → PASS on the overlaid candidate files.

Non-vacuity (inside the leased pool, then restored):

- Parent blob of `kaneo.go` + new tests: compile fail (`userHomeDirFn` undefined). Expected.
- Surgical revert of only the preferred-path block, keeping the new helpers: `TestResolveKaneoProfileCred_PrefersHomeConfig` FAIL (`got key len=19 match=false` — selected the legacy profile). `FallsBackToUserConfigDir` and `EmptyHomeRefusesRelativePath` still PASS.

That is the required RED-then-GREEN for the new preference assertion.

## Residual risk (not blocking)

- Presence of `~/.config/kaneo/config.json` (including empty/malformed) suppresses fallback to the CLI UserConfigDir path. Documented as intentional split from the macOS TCC-protected Application Support file.
- Preference is not GOOS-gated. On Linux with `XDG_CONFIG_HOME` set, a leftover `$HOME/.config/kaneo/config.json` wins over the XDG path. Same class of split-brain the change advertises for macOS.
- `TestResolveKaneoProfileCred_EmptyHomeRefusesRelativePath` stubs home lookup as an error, so it exercises relative `UserConfigDir` refusal more than `IsAbs(home)` on an empty successful home. Production `os.UserHomeDir` with empty HOME also errors on Unix; the IsAbs guard remains extra defense.
- code-review-graph index in this slot was empty (0 nodes at pool HEAD) and was not rebuilt against a mixed tree; callers were taken from `git grep` on the candidate SHA.

PASS: no blocking defect on this exact revision. Credential origin binding and relative-path refusal are preserved; the new non-TCC preference is tested and non-vacuous.
