#!/usr/bin/env zsh
# FAC-826: non-vacuity driver for the resource admission guards.
#
# Runs the focused admission suites, then mutates the REAL production source one
# guard at a time. A mutant counts as killed only when BOTH hold, as separate
# evidence:
#
#   1. it COMPILES, proven by building the test binary, and
#   2. the NAMED killer test fails with the NAMED assertion text, read from
#      `go test -json` rather than scraped from console output.
#
# A compile error, a timeout, a skip, or an unrelated assertion is not a kill.
# Counting any of them is how a vacuous control passes.
#
# UNKNOWN is targeted at the CONSUMER and left unverified INSIDE
# resources.Decide. They are different claims and the driver keeps them apart.
# Neither is proof until the control compiles and its killer fails in CI.
#
# Consumer (targeted, execution pending): the pool gate must refuse a host whose
# readings are UNKNOWN. The fixture host "not-a-known-host" produces UNKNOWN cpu
# and memory readings, Decide refuses on them, and the gate's one admission arm
# is all that acts on that -- neutralised, the switch falls through to its
# admitting default. A single-site regression with a named UNKNOWN killer, and
# it holds however Decide reached the refusal.
#
# Internals (unverified, deliberately): a genuinely UNKNOWN reading is refused at
# three independent sites inside Decide -- checkReading rejects the posture,
# freshness.Value reports ok=false, and the zero value then fails
# Usable/normalizedFrom. No single compiling mutation admits one there, and a
# three-site mutation would not resemble any plausible regression, so that
# clause stays explicitly unverified rather than covered by a fabricated kill.
# The unmeasured-host-not-alert control pins the reporting half only.
#
# The pool-side controls do not depend on runner load: the herdfixture census
# seam pins the PSI, swap and headroom inputs those arms read, so a mutant
# cannot be refused by a busy runner instead of by the guard under test.
#
# All mutation happens in one ephemeral detached worktree this invocation
# creates and owns. The invoking checkout is never written to, and this script
# deletes nothing it did not create.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout jq mktemp; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

# Two sources, two packages. cmd/herd's controls reach the pool gate through a
# subprocess, so they build a CLI binary and need the longer budget.
admission_src=pkg/resources/admission.go
capacity_src=cmd/herd/capacity.go
resources_pkg=./pkg/resources/
herd_pkg=./cmd/herd/

resources_run='TestCPUAndMemoryRefuseIndependently|TestHealthyAdmitsAtTheReserveBoundary|TestKernelPressureRefusesRegardlessOfFreePercent|TestConsumerPolicyEnforcesWhatFreshnessDoesNot|TestUnknownObservationsRefuse|TestStaleObservationsRefuse'
herd_run='TestCapacityRefusesWithoutAnAdmission|TestCapacityAndResourcesRefuseTogether|TestCapacityAdmitsHealthyHost|TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation|TestPoolReviewValidCandidatePreparesSurfaceAndHoldsLease'

# Every baseline run must show these EXACT tests passing at top level. Exit 0
# alone is not a baseline: a selector that matched nothing, or a run whose
# positive control skipped, also exits 0.
resources_expect='TestCPUAndMemoryRefuseIndependently TestHealthyAdmitsAtTheReserveBoundary TestKernelPressureRefusesRegardlessOfFreePercent TestConsumerPolicyEnforcesWhatFreshnessDoesNot TestUnknownObservationsRefuse TestStaleObservationsRefuse'
herd_expect='TestCapacityRefusesWithoutAnAdmission TestCapacityAndResourcesRefuseTogether TestCapacityAdmitsHealthyHost TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation TestPoolReviewValidCandidatePreparesSurfaceAndHoldsLease'

# Finite, explicit, and bounded at both ends before any arithmetic: an absurd or
# overflowing override must be rejected, not added to.
go_timeout=${VERIFY_ADMISSION_GO_TIMEOUT:-600}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_ADMISSION_GO_TIMEOUT, using 600s"
	go_timeout=600
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
cleanup_timeout=60

# Reports are additive. This script creates ONE subdirectory it owns and never
# deletes a directory chosen by a caller: a path being well formed is not
# permission to destroy what is in it.
report_parent=${VERIFY_ADMISSION_REPORT_DIR:-$repo_root/.verify-admission-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work=""
work_owned=0
# work_attempted records that a path was reserved for the checkout. Ownership is
# a stronger claim, set only once `git worktree add` completed.
work_attempted=0
cleanup_failed=0

cleanup() {
	local code=$?
	# Only the checkout THIS invocation created, and only once its creation
	# succeeded. No blind fallback deletion, and no global prune: other
	# worktrees and their metadata are not ours to touch.
	if (( work_owned )) && [[ -n "$work" ]]; then
		if ! timeout -k 10s "${cleanup_timeout}s" \
			git -C "$repo_root" worktree remove --force -- "$work" >&2; then
			print -u2 "error: could not remove the mutation checkout at $work; it is left in place deliberately"
			print -r -- 'cleanup FAILED: the mutation checkout was left in place (see stderr for its path)' >> "$summary"
			cleanup_failed=1
		fi
	elif (( work_attempted )) && [[ -n "$work" && -e "$work" ]]; then
		# Reserved but never owned: the add did not complete, so this run has
		# no claim on what is there. Report the exact residual and fail; do not
		# delete a path whose ownership was never established.
		print -u2 "error: $work exists but this run never took ownership of it; leaving it in place for inspection"
		print -r -- 'residual: a path from an incomplete worktree add was left in place (see stderr for its path)' >> "$summary"
		cleanup_failed=1
	fi
	if (( cleanup_failed )) && (( code == 0 )); then
		code=1
	fi
	return $code
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

note() { print -r -- "$1" >> "$summary"; print -r -- "$1"; }

pin=$(git -C "$repo_root" rev-parse HEAD)
work=$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/tmp}}/verify-admission-XXXXXX")
# mktemp made the directory; `git worktree add` needs the path absent. rmdir
# refuses a non-empty directory, which is the guard we want.
rmdir -- "$work"
work_attempted=1
if ! git -C "$repo_root" worktree add --detach --quiet -- "$work" "$pin"; then
	print -u2 "error: git worktree add did not complete for $work"
	exit 1
fi
work_owned=1

work_pin=$(git -C "$work" rev-parse HEAD)
[[ "$work_pin" == "$pin" ]] || { print -u2 "error: mutation checkout is at $work_pin, not $pin"; exit 1; }

typeset -A pristine
for src in "$admission_src" "$capacity_src"; do
	pristine[$src]=$(git -C "$work" hash-object -- "$src")
	[[ -n "${pristine[$src]}" ]] || { print -u2 "error: cannot hash $src"; exit 1; }
done

# compile_check proves the mutant builds. Its result is kept separately from the
# assertion evidence: "the build broke" and "the test saw the guard" are
# different claims.
compile_check() {
	local pkg=$1 log=$2 exit_code=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$pkg" ) >"$log" 2>&1 || exit_code=$?
	print -r -- "$exit_code"
}

# run_focused executes one -run selection serially and records machine-readable
# events. Mutants run ONLY their anchored killer and its subtests, so an
# unrelated test's failure or timeout cannot contaminate the verdict; the
# baselines run the whole focus set for their package.
run_focused() {
	local pkg=$1 selector=$2 events=$3 console=$4 exit_code=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$pkg" ) >"$events" 2>"$console" || exit_code=$?
	print -r -- "$exit_code"
}

# events_valid proves the whole stream parses BEFORE any predicate reads it. A
# parse failure and a genuine no-match are otherwise the same status, which
# would let a truncated stream read as a clean result.
events_valid() {
	jq -e -s 'type == "array" and length > 0' -- "$1" >/dev/null 2>&1
}

# jq_predicate runs one complete jq test over the validated stream and maps its
# status onto 0 match / 1 no match / 2 evaluation failed.
#
# It is a whole predicate rather than `jq | grep -q` on purpose. Under
# `set -o pipefail` grep exits at its FIRST match, jq dies on SIGPIPE, and the
# pipeline reports 141 even though the match succeeded -- so a long panic
# stream, which is exactly the failure condition, would be classified clean.
jq_predicate() {
	local filter=$1 events=$2
	shift 2
	local predicate_exit=0
	jq -s -e "$filter" "$@" -- "$events" >/dev/null 2>&1 || predicate_exit=$?
	if (( predicate_exit == 0 )); then return 0; fi
	if (( predicate_exit == 1 )); then return 1; fi
	print -u2 "error: jq could not evaluate the event stream (exit $predicate_exit)"
	return 2
}

# stream_broken looks for build, panic, timeout and tool failures across the
# ENTIRE stream, not just the killer's own events: an expected assertion
# followed by a crash somewhere else is not a successful control. Oniguruma
# anchors ^ at line starts, so a marker inside a multi-line Output still hits.
stream_broken() {
	jq_predicate 'any(.[]; (.Action == "output")
		and (((.Output // "") | test("panic: |test timed out|\\[build failed\\]|^# |^signal: |fatal error: "))))' "$1"
}

# test_emitted reports whether the named test or one of its subtests produced an
# event of this action, optionally carrying text. Literal substring, so no
# assertion has to be regex-escaped.
test_emitted() {
	local events=$1 test_name=$2 action=$3 want=${4-}
	if [[ -z "$want" ]]; then
		jq_predicate 'any(.[]; (.Action == $a)
			and (((.Test // "") == $t) or (((.Test // "") | startswith($t + "/")))))' \
			"$events" --arg t "$test_name" --arg a "$action"
		return $?
	fi
	jq_predicate 'any(.[]; (.Action == $a)
		and (((.Test // "") == $t) or (((.Test // "") | startswith($t + "/"))))
		and (((.Output // "") | contains($w))))' \
		"$events" --arg t "$test_name" --arg a "$action" --arg w "$want"
}

# count_occurrences counts LITERAL occurrences, not matching lines.
#
# grep -c counts lines, so an anchor appearing twice on one line counted as 1
# and both copies were then replaced -- a silently doubled mutation that the
# drift guard was supposed to prevent.
count_occurrences() {
	local rest=$1 needle=$2 n=0
	while [[ "$rest" == *"$needle"* ]]; do
		rest=${rest#*"$needle"}
		(( n += 1 ))
	done
	print -r -- "$n"
}

# patch_source replaces exactly one occurrence of anchor. Any other count is a
# hard failure, so source drift cannot become a silent no-op or doubled mutant.
# A read failure is reported distinctly from a count mismatch.
patch_source() {
	local src=$1 anchor=$2 replacement=$3 file=$work/$1 content found mutated
	if ! content=$(<"$file"); then
		print -u2 "error: cannot read $src for mutation"
		return 1
	fi
	found=$(count_occurrences "$content" "$anchor")
	if [[ "$found" != 1 ]]; then
		print -u2 "error: anchor occurs $found time(s) in $src, want exactly 1 (source drifted): $anchor"
		return 1
	fi
	print -r -- "${content//"$anchor"/"$replacement"}" >| "$file"
	mutated=$(git -C "$work" hash-object -- "$src")
	[[ "$mutated" != "${pristine[$src]}" ]] || { print -u2 "error: mutation did not change $src"; return 1; }
}

restore_source() {
	local src=$1 now
	git -C "$work" checkout --quiet -- "$src"
	now=$(git -C "$work" hash-object -- "$src")
	[[ "$now" == "${pristine[$src]}" ]] || { print -u2 "error: restore left $src at $now, want ${pristine[$src]}"; return 1; }
}

restore_all() {
	local src
	for src in "$admission_src" "$capacity_src"; do
		restore_source "$src"
	done
}

# baseline_ok requires a validated stream in which every EXPECTED test emitted a
# top-level pass, and none of them skipped or failed. Exit 0 is not enough: a
# selector that matched nothing exits 0, and so does a run whose positive
# control skipped itself.
baseline_ok() {
	local events=$1 expected=$2 name probe missing=0
	if ! events_valid "$events"; then
		note "  stream is not valid JSON"
		return 1
	fi
	for name in ${=expected}; do
		probe=0; test_emitted_exact "$events" "$name" pass || probe=$?
		case $probe in
			1) note "  $name did not pass at top level"; missing=1 ;;
			2) note "  $name could not be evaluated"; missing=1 ;;
		esac
		probe=0; test_emitted "$events" "$name" skip || probe=$?
		if (( probe == 0 )); then note "  $name skipped"; missing=1; fi
		probe=0; test_emitted "$events" "$name" fail || probe=$?
		if (( probe == 0 )); then note "  $name failed"; missing=1; fi
	done
	return $missing
}

# test_emitted_exact binds to the test itself, never a subtest: a baseline is
# about the named test passing, not about one of its cases doing so.
test_emitted_exact() {
	jq_predicate 'any(.[]; (.Action == $a) and ((.Test // "") == $t))' \
		"$1" --arg t "$2" --arg a "$3"
}

# classify_run maps one mutant onto the contract, given a compile that already
# passed. Only an exact go test exit 1, over a stream with no tool failure
# anywhere in it, naming the killer and carrying the expected text, is a kill.
classify_run() {
	local run_exit=$1 events=$2 killer=$3 want=$4 probe
	if ! events_valid "$events"; then print -r -- 'INVALID-EVENTS'; return; fi
	if (( run_exit == 0 )); then print -r -- 'SURVIVED'; return; fi
	if (( run_exit != 1 )); then print -r -- "TOOLFAIL(exit $run_exit)"; return; fi

	# probe is reset before every predicate: a leftover status from the previous
	# question would answer the next one.
	probe=0; stream_broken "$events" || probe=$?
	case $probe in
		0) print -r -- 'BROKEN-RUN'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac

	probe=0; test_emitted "$events" "$killer" skip || probe=$?
	case $probe in
		0) print -r -- 'SKIPPED'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac

	probe=0; test_emitted "$events" "$killer" fail || probe=$?
	case $probe in
		1) print -r -- 'WRONG-TEST'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac

	probe=0; test_emitted "$events" "$killer" output "$want" || probe=$?
	case $probe in
		1) print -r -- 'WRONG-ASSERTION'; return ;;
		2) print -r -- 'EVENTS-UNREADABLE'; return ;;
	esac

	print -r -- 'KILLED'
}

# id | source | anchor | replacement | anchor2 | replacement2 | package | killer | assertion
#
# anchor2 is empty for a single-site mutation. A pair is used only where the
# guard is genuinely defended in two places and removing one would panic or be
# caught by the other: that is still ONE introduced regression, not two.
#
# Three rows share the capacity anchor on purpose. They are separate CAUSAL
# claims -- cpu, memory pressure and UNKNOWN each reaching the pool gate -- and
# each is bound to its own subtest, so a kill in one cannot be reported as
# evidence for another.
sep=$'\x1f'
mutations=(
"cpu-threshold-removed${sep}${admission_src}${sep}		if normalized >= limits.CPURefuseLoad {${sep}		if false { // MUTANT: cpu threshold removed${sep}${sep}${sep}${resources_pkg}${sep}TestCPUAndMemoryRefuseIndependently${sep}saturated cpu with healthy memory admitted"
"memory-reserve-removed${sep}${admission_src}${sep}		case head.FreePctGates && head.FreePct < limits.MemReservePct:${sep}		case false: // MUTANT: os reserve removed${sep}${sep}${sep}${resources_pkg}${sep}TestHealthyAdmitsAtTheReserveBoundary${sep}headroom one point below the reserve admitted"
"kernel-pressure-ignored${sep}${admission_src}${sep}		case head.Pressure.Unsafe():${sep}		case false: // MUTANT: kernel pressure ignored${sep}${sep}${sep}${resources_pkg}${sep}TestKernelPressureRefusesRegardlessOfFreePercent${sep}admitted at 95% free"
"stale-age-never-expires${sep}${admission_src}${sep}	if age := now.Sub(observedAt); age > limits.StaleAfter {${sep}	if age := now.Sub(observedAt); false { // MUTANT: age never expires${sep}${sep}${sep}${resources_pkg}${sep}TestConsumerPolicyEnforcesWhatFreshnessDoesNot${sep}an hour-old FRESH reading admitted"
"unrecognized-posture-accepted${sep}${admission_src}${sep}		return fmt.Sprintf(\"%s carries an unrecognized freshness state %q; an unset posture is not an observation\", what, string(state))${sep}		return \"\" // MUTANT: unrecognised posture accepted${sep}${sep}${sep}${resources_pkg}${sep}TestConsumerPolicyEnforcesWhatFreshnessDoesNot${sep}an unrecognized freshness state admitted"
"unmeasured-host-not-alert${sep}${admission_src}${sep}	if !a.cpuUsable || !a.memUsable {${sep}	if false { // MUTANT: an unmeasured host stops reporting ALERT${sep}${sep}${sep}${resources_pkg}${sep}TestUnknownObservationsRefuse${sep}want \"ALERT\" for an unmeasured host"
"capacity-ignores-refusal${sep}${capacity_src}${sep}	case !o.Admission.Admits:${sep}	case false: // MUTANT: pool gate ignores the shared refusal${sep}${sep}${sep}${herd_pkg}${sep}TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation/cpu-saturated${sep}an unsafe host must refuse the launch"
"capacity-ignores-unknown-host${sep}${capacity_src}${sep}	case !o.Admission.Admits:${sep}	case false: // MUTANT: pool gate ignores the shared refusal${sep}${sep}${sep}${herd_pkg}${sep}TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation/not-a-known-host${sep}an unsafe host must refuse the launch"
"capacity-ignores-memory-pressure${sep}${capacity_src}${sep}	case !o.Admission.Admits:${sep}	case false: // MUTANT: pool gate ignores the shared refusal${sep}${sep}${sep}${herd_pkg}${sep}TestPoolReviewRefusesUnsafeHostBeforeCandidatePreparation/memory-pressure${sep}an unsafe host must refuse the launch"
"capacity-admits-without-decision${sep}${capacity_src}${sep}	case o.admission == nil || o.Admission == nil:${sep}	case false: // MUTANT: unevaluated observation admitted${sep}	case !o.Admission.Admits:${sep}	case false: // MUTANT: paired, so the nil case cannot be dereferenced${sep}${herd_pkg}${sep}TestCapacityRefusesWithoutAnAdmission${sep}an unevaluated observation admitted"
)

note "pin $pin"
if [[ "$run_dir" == "$repo_root"/* ]]; then
	note "report ${run_dir#$repo_root/}"
else
	note "report ${run_dir:t} (under VERIFY_ADMISSION_REPORT_DIR, outside the repository)"
fi
for src in "$admission_src" "$capacity_src"; do
	note "source $src (${pristine[$src]})"
done


baseline_failed=0
for spec in "${resources_pkg}${sep}${resources_run}${sep}resources${sep}${resources_expect}" "${herd_pkg}${sep}${herd_run}${sep}herd${sep}${herd_expect}"; do
	fields=("${(@ps:$sep:)spec}")
	baseline_exit=$(run_focused "$fields[1]" "$fields[2]" "$run_dir/baseline-$fields[3].json" "$run_dir/baseline-$fields[3].err")
	if (( baseline_exit != 0 )); then
		note "baseline $fields[3] FAILED (exit $baseline_exit) - the suite must pass before any mutant means anything"
		baseline_failed=1
	elif ! baseline_ok "$run_dir/baseline-$fields[3].json" "$fields[4]"; then
		note "baseline $fields[3] FAILED - exit 0 but the required tests did not all pass"
		baseline_failed=1
	else
		note "baseline $fields[3] PASS (every expected test passed at top level)"
	fi
done
(( baseline_failed == 0 )) || exit 1

failures=0
index=0
for record in "${mutations[@]}"; do
	(( index += 1 ))
	fields=("${(@ps:$sep:)record}")
	id=$fields[1]; src=$fields[2]; anchor=$fields[3]; replacement=$fields[4]
	anchor2=$fields[5]; replacement2=$fields[6]
	pkg=$fields[7]; killer=$fields[8]; want=$fields[9]
	stem=$run_dir/$(printf 'm%02d-%s' "$index" "$id")

	patch_source "$src" "$anchor" "$replacement"
	if [[ -n "$anchor2" ]]; then
		patch_source "$src" "$anchor2" "$replacement2"
	fi
	compile_exit=$(compile_check "$pkg" "$stem.compile.log")
	if (( compile_exit != 0 )); then
		restore_source "$src"
		note "mutant $id: COMPILE-FAIL (exit $compile_exit) - a mutant that does not build proves nothing"
		(( failures += 1 ))
		continue
	fi
	# Anchored to this mutant's one killer, subtests included.
	run_exit=$(run_focused "$pkg" "^${killer}$" "$stem.json" "$stem.err")
	verdict=$(classify_run "$run_exit" "$stem.json" "$killer" "$want")
	restore_source "$src"

	note "mutant $id: compile PASS, run $verdict (killer $killer, exit $run_exit)"
	if [[ "$verdict" != KILLED ]]; then
		(( failures += 1 ))
		[[ -s "$stem.err" ]] && tail -n 20 -- "$stem.err" >&2
	fi
done

restore_all
restored_failed=0
for spec in "${resources_pkg}${sep}${resources_run}${sep}resources${sep}${resources_expect}" "${herd_pkg}${sep}${herd_run}${sep}herd${sep}${herd_expect}"; do
	fields=("${(@ps:$sep:)spec}")
	restored_exit=$(run_focused "$fields[1]" "$fields[2]" "$run_dir/restored-$fields[3].json" "$run_dir/restored-$fields[3].err")
	if (( restored_exit != 0 )); then
		note "restored baseline $fields[3] FAILED (exit $restored_exit) - the source did not come back clean"
		restored_failed=1
	elif ! baseline_ok "$run_dir/restored-$fields[3].json" "$fields[4]"; then
		note "restored baseline $fields[3] FAILED - exit 0 but the required tests did not all pass"
		restored_failed=1
	else
		note "restored baseline $fields[3] PASS (every expected test passed at top level)"
	fi
done
(( restored_failed == 0 )) || exit 1

if (( failures > 0 )); then
	note "$failures of ${#mutations[@]} controls did not kill their mutant"
	exit 1
fi
note "all ${#mutations[@]} controls compiled and died on their named assertion"
