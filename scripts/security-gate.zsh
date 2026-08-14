#!/usr/bin/env zsh
# FAC-251: deterministic, root-scoped source security gate.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"
(( $+commands[gosec] )) || { print -u2 'error: pinned gosec is required'; exit 1; }
(( $+commands[jq] )) || { print -u2 'error: jq is required'; exit 1; }

# Copy only paths Git currently tracks.  In particular, do not walk the
# checkout: active .worktrees and .herd trees must be immaterial to this gate.
scan_root=$(mktemp -d)
report=$(mktemp)
cleanup() { find "$scan_root" -depth -delete 2>/dev/null || true; rm -f "$report"; }
trap cleanup EXIT
while IFS= read -r -d '' file_path; do
	case "$file_path" in .worktrees/*|.herd/*) continue;; esac
	mkdir -p "$scan_root/${file_path:h}"
	cp -p "$file_path" "$scan_root/$file_path"
done < <(git ls-files -z)

(cd "$scan_root" && gosec -fmt=json -out="$report" --no-fail ./... >/dev/null 2>&1 || true)

baseline=security/baselines/gosec-high.tsv
[[ -f "$baseline" ]] || { print -u2 "error: missing $baseline"; exit 1; }
today=$(date -u +%F)
typeset -A expected seen
while IFS=$'\t' read -r rule file line fingerprint rationale owner expiry; do
	[[ "$rule" == \#* || -z "$rule" ]] && continue
	[[ -n "$rule" && -n "$file" && -n "$line" && -n "$fingerprint" && -n "$rationale" && -n "$owner" && -n "$expiry" ]] || { print -u2 "error: malformed baseline entry"; exit 1; }
	[[ "$expiry" > "$today" ]] || { print -u2 "error: expired baseline entry $fingerprint ($expiry)"; exit 1; }
	expected[$fingerprint]=1
done < "$baseline"

while IFS=$'\t' read -r rule file line; do
	file=${file#$scan_root/}
	fingerprint=$(print -rn -- "$rule|$file|$line" | shasum -a 256 | awk '{print $1}')
	if [[ -z ${expected[$fingerprint]-} ]]; then
		print -u2 "error: unreviewed HIGH finding: $rule $file:$line fingerprint=$fingerprint"
		exit 1
	fi
	seen[$fingerprint]=1
done < <(jq -r --arg root "$scan_root/" '.Issues[] | select((.severity == "HIGH" or .severity == "CRITICAL") and (.file | startswith($root))) | [.rule_id,.file,.line] | @tsv' "$report" | LC_ALL=C sort)

for fingerprint in ${(k)expected}; do
	[[ -n ${seen[$fingerprint]-} ]] || { print -u2 "error: stale baseline entry $fingerprint"; exit 1; }
done
print '==> FAC-251 gosec HIGH/CRITICAL baseline is exact and current'

(( $+commands[gitleaks] )) || { print -u2 'error: gitleaks is required'; exit 1; }
leaks_baseline=${GITLEAKS_BASELINE:-security/baselines/gitleaks.tsv}
[[ -f "$leaks_baseline" ]] || { print -u2 "error: missing $leaks_baseline"; exit 1; }
leaks_report=$(mktemp)
gitleaks git --no-banner --redact=100 --report-format json --report-path "$leaks_report" . >/dev/null 2>&1 || true
typeset -A leak_expected leak_seen
while IFS=$'\t' read -r fingerprint classification owner expiry; do
	[[ "$fingerprint" == \#* || -z "$fingerprint" ]] && continue
	[[ -n "$classification" && -n "$owner" && "$expiry" > "$today" ]] || { print -u2 "error: malformed or expired gitleaks baseline entry $fingerprint"; exit 1; }
	leak_expected[$fingerprint]=1
done < "$leaks_baseline"
while IFS= read -r fingerprint; do
	[[ -n ${leak_expected[$fingerprint]-} ]] || { print -u2 "error: unreviewed gitleaks finding $fingerprint"; exit 1; }
	leak_seen[$fingerprint]=1
done < <(jq -r '.[].Fingerprint' "$leaks_report" | LC_ALL=C sort)
for fingerprint in ${(k)leak_expected}; do
	[[ -n ${leak_seen[$fingerprint]-} ]] || { print -u2 "error: stale gitleaks baseline entry $fingerprint"; exit 1; }
done
rm -f "$leaks_report"
print '==> FAC-251 gitleaks history baseline is exact and current'
