#!/usr/bin/env zsh
# FAC-251 non-vacuity checks for the deterministic tracked-source gate.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
cleanup() { find "$tmp" -depth -delete 2>/dev/null || true; }
trap cleanup EXIT
mkdir "$tmp/repo"
cd "$tmp/repo"
git init -q
git config user.email fac251@example.invalid
git config user.name fac251
git config commit.gpgsign false
git config core.hooksPath /dev/null
# This is a new, minimal tracked Go repository. Its Gitleaks baseline is
# intentionally empty; production history belongs only to the production
# baseline and must not make this non-vacuity fixture pass by accident.
mkdir -p scripts security/baselines
cp -p "$root/scripts/security-gate.zsh" scripts/security-gate.zsh
print 'module example.invalid/fac251fixture' > go.mod
print 'package fixture' > fixture.go
print '# rule\tfile\tline\tfingerprint\trationale\towner\texpiry' > security/baselines/gosec-high.tsv
git add -A
git -c commit.gpgsign=false commit -qm 'test: current FAC-251 gate fixture'
print '# fingerprint\tclassification\towner\texpiry' > security/baselines/gitleaks.tsv
mkdir -p .worktrees/live
# Proven by a disposable `gitleaks git` fixture: the assembled value matches
# github-pat, but no token-shaped literal is committed with this test harness.
token_prefix='ghp_'
token_suffix='012345678901234567890123456789012345'
print -- "github_token = \"${token_prefix}${token_suffix}\"" > .worktrees/live/secret.txt
./scripts/security-gate.zsh >/dev/null

# A syntactically valid finding that gosec did not report must fail closed as
# stale; every stale entry must be diagnosed in one run (FAC-285). After their
# removal, the current empty fixture baseline passes again.
stale_fp1=$(print -rn -- 'G702|fixture.go|999' | shasum -a 256 | awk '{print $1}')
stale_fp2=$(print -rn -- 'G703|fixture.go|888' | shasum -a 256 | awk '{print $1}')
print -- "G702\tfixture.go\t999\t$stale_fp1\tfixture stale-entry regression 1\tsecurity-maintainers\t2026-12-31" >> security/baselines/gosec-high.tsv
print -- "G703\tfixture.go\t888\t$stale_fp2\tfixture stale-entry regression 2\tsecurity-maintainers\t2026-12-31" >> security/baselines/gosec-high.tsv
stale_report="$tmp/stale-gosec.out"
if ./scripts/security-gate.zsh >"$stale_report" 2>&1; then exit 1; fi
grep -F -- "error: stale baseline entry $stale_fp1" "$stale_report" >/dev/null || exit 1
grep -F -- "error: stale baseline entry $stale_fp2" "$stale_report" >/dev/null || exit 1
print '# rule\tfile\tline\tfingerprint\trationale\towner\texpiry' > security/baselines/gosec-high.tsv
./scripts/security-gate.zsh >/dev/null

print -- "github_token = \"${token_prefix}${token_suffix}\"" > tracked-secret.txt
git add tracked-secret.txt
git -c commit.gpgsign=false commit -qm 'test: introduce detected secret'
fixture_report="$tmp/gitleaks.json"
gitleaks git --no-banner --redact=100 --report-format json --report-path "$fixture_report" . >/dev/null 2>&1 || true
fingerprint=$(jq -r '.[].Fingerprint' "$fixture_report" | LC_ALL=C sort -u | head -n 1)
[[ -n "$fingerprint" && "$fingerprint" != null ]] || { print -u2 'error: injected secret was not detected'; exit 1; }
print '# fingerprint\tclassification\towner\texpiry' > "$tmp/expired.tsv"
print -- "$fingerprint\tfixture\tsecurity-maintainers\t2000-01-01" >> "$tmp/expired.tsv"
if GITLEAKS_BASELINE="$tmp/expired.tsv" ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi
if GITLEAKS_BASELINE="$tmp/missing.tsv" ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi
if ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi
print '==> FAC-251 security negative tests passed'
