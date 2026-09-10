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
	typeset -i gosec_failures=0
	local gosec_last_status=0
	# Module reports live INSIDE the owned scan root: the gate's existing
	# EXIT trap deletes scan_root on every exit path, including a
	# run_with_timeout SIGKILL of this function (whose own traps cannot
	# run), so no module report is ever leaked outside owned space.
	mkdir -p "$scan_root/.gosec-reports"
	for gomod in go.mod **/go.mod; do
		[[ -f "$gomod" ]] || continue
		mdir="${gomod:h}"
		subrep=$(mktemp "$scan_root/.gosec-reports/report.XXXXXX")
		subreports+=( "$subrep" )
		cd "$scan_root/$mdir"
		# A module failure is recorded, not immediately fatal: remaining
		# modules are still scanned. The recorded failure fails the gate
		# below, so an operational gosec failure can never be evaluated
		# as zero findings.
		gosec -fmt=json -out="$subrep" --no-fail -exclude=G701,G702,G703,G704,G705,G706,G707,G708,G709,G710 ./... >/dev/null 2>&1 || { gosec_last_status=$?; gosec_failures=1; }
		cd "$scan_root"
	done
	# The per-module failure recording intentionally keeps the scan
	# running when individual modules fail, so a crashed gosec surfaces
	# only as an empty, truncated, malformed, or multi-document
	# subreport. jq -s succeeds on zero input documents, and a findings
	# pipeline over such output evaluates no findings. Every subreport
	# must therefore be exactly ONE complete JSON document. The pinned
	# gosec v2.22.10 emits, for no findings, a JSON OBJECT whose "Issues"
	# member is null (an empty Go slice marshals as null); that
	# representation is accepted. A bare top-level null is the Gitleaks
	# no-findings contract, not gosec's, and is rejected here.
	for subrep in "${subreports[@]}"; do
		if [[ ! -s "$subrep" ]] || ! jq -e -s 'length == 1 and (.[0] | type == "object") and (.[0].Issues | type == "null" or type == "array")' "$subrep" >/dev/null 2>&1; then
			print -u2 'error: gosec produced no complete single-document JSON report'
			return 1
		fi
	done
	if (( gosec_failures )); then
		print -u2 "error: gosec scanner failed with exit status $gosec_last_status"
		return 1
	fi
	jq -s '{Issues: ((map(.Issues // []) | add) // [])}' "${subreports[@]}" > "$report"
	rm -f "${subreports[@]}"
	rmdir "$scan_root/.gosec-reports" 2>/dev/null || true
}

gosec_status=0
run_with_timeout "$gosec_timeout" "gosec" run_gosec || gosec_status=$?
if (( gosec_status == 124 )); then
	exit 1
fi
if (( gosec_status != 0 )); then
	print -u2 "error: gosec scanner failed with exit status $gosec_status"
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
# A no-findings gosec report is an object with a null Issues member (the
# pinned v2.22.10 representation), so iterate over the null-coalesced
# array: null yields no findings instead of a jq iteration error.
jq -r --arg root "$scan_root/" '(.Issues // [])[] | select((.severity == "HIGH" or .severity == "CRITICAL") and (.file | startswith($root))) | [.rule_id,.file,.line] | @tsv' "$report" | LC_ALL=C sort > "$findings"
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
if (( gitleaks_status != 0 && gitleaks_status != 1 )); then
	print -u2 "error: gitleaks scanner failed with exit status $gitleaks_status"
	exit 1
fi
if [[ ! -s "$leaks_report" ]] || ! jq -e -s '
	length == 1 and
	(.[0] | type == "null" or
		(type == "array" and all(.[]; type == "object" and (.Fingerprint | type == "string") and (.Fingerprint | length > 0))))
' "$leaks_report" >/dev/null; then
	print -u2 'error: gitleaks produced no complete JSON null/array report'
	exit 1
fi
# FAC-660 permits a complete JSON null as the scanner's no-finding result.
# Normalize that representation only after the complete-report check; empty,
# malformed, and multi-document output remains a hard error above.
report_count=$(jq -r 'if type == "null" then 0 else length end' "$leaks_report")
if (( gitleaks_status == 0 && report_count != 0 )); then
	print -u2 "error: gitleaks returned 0 with $report_count finding(s)"
	exit 1
fi
if (( gitleaks_status == 1 && report_count == 0 )); then
	print -u2 'error: gitleaks returned 1 without findings'
	exit 1
fi
typeset -A leak_expected leak_seen
while IFS=$'\t' read -r fingerprint classification owner expiry; do
	[[ "$fingerprint" == \#* || -z "$fingerprint" ]] && continue
	[[ -n "$classification" && -n "$owner" && "$expiry" > "$today" ]] || { print -u2 "error: malformed or expired gitleaks baseline entry $fingerprint"; exit 1; }
	leak_expected[$fingerprint]=1
done < "$leaks_baseline"

leaks_findings=$(mktemp)
if ! jq -r 'if type == "null" then [] else . end | .[].Fingerprint' "$leaks_report" | LC_ALL=C sort > "$leaks_findings"; then
	print -u2 'error: could not parse gitleaks finding fingerprints'
	exit 1
fi
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
