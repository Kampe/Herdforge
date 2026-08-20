#!/usr/bin/env zsh
# FAC-251: deterministic, root-scoped source security gate.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"
(( $+commands[gosec] )) || { print -u2 'error: pinned gosec is required'; exit 1; }
(( $+commands[jq] )) || { print -u2 'error: jq is required'; exit 1; }

# Default to 300s (5m) to accommodate repository scale and multi-package AST
# traversal on shared CI runners without masking hangs; preserve environment overrides.
gosec_timeout=${GOSEC_TIMEOUT:-${SECURITY_GATE_TIMEOUT:-300}}
gitleaks_timeout=${GITLEAKS_TIMEOUT:-${SECURITY_GATE_TIMEOUT:-300}}
if ! [[ "$gosec_timeout" =~ ^[1-9][0-9]*$ ]]; then
	gosec_timeout=300
fi
if ! [[ "$gitleaks_timeout" =~ ^[1-9][0-9]*$ ]]; then
	gitleaks_timeout=300
fi

# Copy only paths Git currently tracks.  In particular, do not walk the
# checkout: active .worktrees and .herd trees must be immaterial to this gate.
scan_root=""
report=""
findings=""
leaks_findings=""
leaks_report=""
current_child_pid=""
current_timer_pid=""
current_timeout_marker=""

cleanup() {
	if [[ -n "${current_timer_pid-}" ]] && kill -0 "$current_timer_pid" 2>/dev/null; then
		kill -9 "$current_timer_pid" 2>/dev/null || true
	fi
	if [[ -n "${current_child_pid-}" ]] && kill -0 "$current_child_pid" 2>/dev/null; then
		pkill -9 -P "$current_child_pid" 2>/dev/null || true
		kill -9 "$current_child_pid" 2>/dev/null || true
	fi
	[[ -n "$scan_root" ]] && { find "$scan_root" -depth -delete 2>/dev/null || true; }
	rm -f "$report" "$findings" "$leaks_findings" "$leaks_report" "${current_timeout_marker:-}"
}
trap cleanup EXIT INT TERM

# run_with_timeout <timeout_seconds> <label> <command> [args...]
# Executes a command with a strict timeout. If the command exceeds the timeout,
# it and its children are killed (SIGTERM then SIGKILL), a diagnostic is printed
# to stderr, and the function returns 124.
run_with_timeout() {
	local timeout_sec="$1"
	local label="$2"
	shift 2
	local -a cmd=("$@")

	current_timeout_marker=$(mktemp)
	rm -f "$current_timeout_marker"

	"${cmd[@]}" &
	current_child_pid=$!

	(
		local elapsed=0
		while (( elapsed < timeout_sec )); do
			sleep 1
			(( elapsed++ ))
			if ! kill -0 "$current_child_pid" 2>/dev/null; then
				exit 0
			fi
		done
		touch "$current_timeout_marker"
		pkill -P "$current_child_pid" 2>/dev/null || true
		kill -TERM "$current_child_pid" 2>/dev/null || true
		sleep 0.5 2>/dev/null || sleep 1
		pkill -9 -P "$current_child_pid" 2>/dev/null || true
		kill -9 "$current_child_pid" 2>/dev/null || true
	) &
	current_timer_pid=$!

	local exit_code=0
	wait "$current_child_pid" 2>/dev/null || exit_code=$?
	current_child_pid=""

	if kill -0 "$current_timer_pid" 2>/dev/null; then
		kill -9 "$current_timer_pid" 2>/dev/null || true
		wait "$current_timer_pid" 2>/dev/null || true
	fi
	current_timer_pid=""

	if [[ -f "$current_timeout_marker" ]]; then
		rm -f "$current_timeout_marker"
		current_timeout_marker=""
		print -u2 "error: $label timed out after ${timeout_sec}s"
		return 124
	fi
	rm -f "$current_timeout_marker"
	current_timeout_marker=""
	return "$exit_code"
}

scan_root=$(mktemp -d)
report=$(mktemp)
typeset -a tracked_files
tracked_files=( ${(0)"$(git ls-files -z)"} )
for file_path in "${tracked_files[@]}"; do
	[[ -z "$file_path" ]] && continue
	case "$file_path" in .worktrees/*|.herd/*) continue;; esac
	mkdir -p "$scan_root/${file_path:h}"
	cp -p "$file_path" "$scan_root/$file_path"
done

run_gosec() {
	cd "$scan_root"
	typeset -a subreports
	subreports=()
	local mdir subrep
	for gomod in go.mod **/go.mod; do
		[[ -f "$gomod" ]] || continue
		mdir="${gomod:h}"
		subrep=$(mktemp)
		subreports+=( "$subrep" )
		cd "$scan_root/$mdir"
		gosec -fmt=json -out="$subrep" --no-fail -exclude=G701,G702,G703,G704,G705,G706,G707,G708,G709,G710 ./... >/dev/null 2>&1 || true
		cd "$scan_root"
	done
	jq -s '{Issues: (map(.Issues // []) | add)}' "${subreports[@]}" > "$report"
	rm -f "${subreports[@]}"
}

gosec_status=0
run_with_timeout "$gosec_timeout" "gosec" run_gosec || gosec_status=$?
if (( gosec_status == 124 )); then
	exit 1
fi

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

findings=$(mktemp)
jq -r --arg root "$scan_root/" '.Issues[] | select((.severity == "HIGH" or .severity == "CRITICAL") and (.file | startswith($root))) | [.rule_id,.file,.line] | @tsv' "$report" | LC_ALL=C sort > "$findings"
while IFS=$'\t' read -r rule file line; do
	file=${file#$scan_root/}
	fingerprint=$(print -rn -- "$rule|$file|$line" | shasum -a 256 | awk '{print $1}')
	if [[ -z ${expected[$fingerprint]-} ]]; then
		print -u2 "error: unreviewed HIGH finding: $rule $file:$line fingerprint=$fingerprint"
		exit 1
	fi
	seen[$fingerprint]=1
done < "$findings"
rm -f "$findings"

typeset -a stale
for fingerprint in ${(k)expected}; do
	[[ -n ${seen[$fingerprint]-} ]] || stale+=( "$fingerprint" )
done
if (( ${#stale} > 0 )); then
	for fingerprint in ${(o)stale}; do
		print -u2 "error: stale baseline entry $fingerprint"
	done
	exit 1
fi
print '==> FAC-251 gosec HIGH/CRITICAL baseline is exact and current'

(( $+commands[gitleaks] )) || { print -u2 'error: gitleaks is required'; exit 1; }
leaks_baseline=${GITLEAKS_BASELINE:-security/baselines/gitleaks.tsv}
[[ -f "$leaks_baseline" ]] || { print -u2 "error: missing $leaks_baseline"; exit 1; }
leaks_report=$(mktemp)
run_gitleaks() {
	gitleaks git --no-banner --redact=100 --report-format json --report-path "$leaks_report" . >/dev/null 2>&1
}
gitleaks_status=0
run_with_timeout "$gitleaks_timeout" "gitleaks" run_gitleaks || gitleaks_status=$?
if (( gitleaks_status == 124 )); then
	exit 1
fi
typeset -A leak_expected leak_seen
while IFS=$'\t' read -r fingerprint classification owner expiry; do
	[[ "$fingerprint" == \#* || -z "$fingerprint" ]] && continue
	[[ -n "$classification" && -n "$owner" && "$expiry" > "$today" ]] || { print -u2 "error: malformed or expired gitleaks baseline entry $fingerprint"; exit 1; }
	leak_expected[$fingerprint]=1
done < "$leaks_baseline"

leaks_findings=$(mktemp)
jq -r '.[].Fingerprint' "$leaks_report" | LC_ALL=C sort > "$leaks_findings"
while IFS= read -r fingerprint; do
	[[ -n ${leak_expected[$fingerprint]-} ]] || { print -u2 "error: unreviewed gitleaks finding $fingerprint"; exit 1; }
	leak_seen[$fingerprint]=1
done < "$leaks_findings"
rm -f "$leaks_findings"

# FAC-535: a baseline row is keyed commit:file:rule:line, and every PR merges
# with rebase, which REWRITES commit SHAs. A row whose commit is no longer
# reachable from HEAD can never be produced by `gitleaks git .` again, so
# treating it as "stale" is wrong — it is simply unscannable here, and the same
# row may still be reachable in a differently-cloned checkout (CI vs a local
# single-branch clone). Skip those rows instead of erroring; only a row whose
# commit IS reachable and yet went unseen is genuinely stale.
typeset -a leak_stale leak_unreachable
for fingerprint in ${(k)leak_expected}; do
	[[ -n ${leak_seen[$fingerprint]-} ]] && continue
	if git cat-file -e "${fingerprint%%:*}^{commit}" 2>/dev/null && \
		git merge-base --is-ancestor "${fingerprint%%:*}" HEAD 2>/dev/null; then
		leak_stale+=( "$fingerprint" )
	else
		leak_unreachable+=( "$fingerprint" )
	fi
done
if (( ${#leak_unreachable} > 0 )); then
	for fingerprint in ${(o)leak_unreachable}; do
		print -u2 "note: gitleaks baseline entry not scannable in this checkout (rebased/unreachable commit): $fingerprint"
	done
fi
if (( ${#leak_stale} > 0 )); then
	for fingerprint in ${(o)leak_stale}; do
		print -u2 "error: stale gitleaks baseline entry $fingerprint"
	done
	exit 1
fi
rm -f "$leaks_report"
print '==> FAC-251 gitleaks history baseline is exact and current'
