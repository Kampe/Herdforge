#!/usr/bin/env zsh
# FAC-764: non-vacuity driver for the pool's owning-root anchoring.
#
# Release and reclaimDeadLocked handed the STORED slot.Path to `git -C`, and
# git resolves a -C argument against the process's own working directory. A
# slot path persisted relative — which Ensure does whenever the pool was
# created through a relative pool root — therefore named a different directory
# for every caller. From the owning repository it worked; from anywhere else it
# either failed, or silently reset whatever sat at that relative path under the
# caller.
#
# The tests pass. That says nothing about whether they would NOTICE the
# anchoring being removed. This driver removes each guard from the REAL
# production source one at a time and requires the named oracle to die on its
# named assertion.
#
# A mutant counts as KILLED only when ALL of these hold, as separate evidence:
#
#   1. it COMPILES, proven by building the test binary in its own phase,
#   2. the run exits NON-ZERO,
#   3. the NAMED killer test emits a `fail` action, and
#   4. that test's own output carries the NAMED assertion,
#
# all read from `go test -json`. A compile error, a panic, a Go-internal test
# timeout, an external timeout, a skip, or an unrelated assertion is NOT a kill
# and is reported under its own verdict, so a vacuous control can never be
# counted as proof.
#
# Every control's definition, field count and LIVE anchor uniqueness is
# validated BEFORE the baseline runs, so a stale registry fails fast instead of
# spending a baseline and then reporting a broken control as a survivor.
#
# Mutation happens in one ephemeral detached worktree this invocation creates
# and owns, placed outside the evidence directory so a failed cleanup can never
# be uploaded as an artifact. The invoking checkout is never written to, and
# this script deletes nothing it did not create.
set -euo pipefail

script_dir=${0:A:h}
repo_root=$(git -C "$script_dir/.." rev-parse --show-toplevel)
cd "$repo_root"

for tool in git go timeout jq mktemp rmdir; do
	(( $+commands[$tool] )) || { print -u2 "error: $tool is required"; exit 1; }
done

pool_src=pkg/worktree/pool.go
worktree_pkg=./pkg/worktree/

pool_run='TestReleaseFromAForeignCaller|TestReclaimFromAForeignCaller|TestAReassignedSlotRefuses|TestReleaseRefuses|TestReleaseKeepsTheLease'
pool_expect='TestReleaseFromAForeignCallerResetsTheOwningSlot TestReclaimFromAForeignCallerResetsTheOwningSlot TestAReassignedSlotRefusesTheRetiredLeaseIdentity TestReleaseRefusesAnUnknownLeaseAndLeavesTheOwnerHeld TestReleaseRefusesAStoredPathOutsideThePoolRootAndKeepsTheLease TestReleaseRefusesAnUnregisteredPathAndKeepsTheLease TestReleaseKeepsTheLeaseWhenTheSlotIsGone TestRelativeConstructorAnchorsBeforeTheCallerMoves TestFixedClockMintsAUniqueIncarnationForEveryAssignment TestARecreatedSlotDoesNotResurrectARetiredIdentity TestLegacyStateDoesNotReissueAReleasedGeneration TestLegacyHeldSlotDoesNotReissueItsOwnGenerationAfterReclaim TestStructLiteralPoolRefusesRelativeRootsOnItsFirstStateRead'

go_timeout=${VERIFY_POOL_GO_TIMEOUT:-300}
if [[ "$go_timeout" != <-> ]] || (( ${#go_timeout} > 4 )) || (( go_timeout < 60 || go_timeout > 1800 )); then
	print -u2 "warning: ignoring unusable VERIFY_POOL_GO_TIMEOUT, using 300s"
	go_timeout=300
fi
wall_timeout=$(( go_timeout + 120 ))
compile_timeout=$go_timeout
worktree_timeout=120
cleanup_timeout=60

report_parent=${VERIFY_POOL_REPORT_DIR:-$repo_root/.verify-pool-owning-root-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")
summary=$run_dir/summary.txt
: >| "$summary"

work_parent=$(mktemp -d)
work=""
work_owned=0
work_attempted=0
cleanup_done=0
cleanup_rc=0

note() { print -r -- "$@" | tee -a "$summary"; }

cleanup_worktree() {
	if (( cleanup_done )); then
		return $cleanup_rc
	fi
	cleanup_done=1
	if (( work_owned )) && [[ -n "$work" ]]; then
		if timeout -k 10s "${cleanup_timeout}s" \
			git -C "$repo_root" worktree remove --force "$work" >/dev/null 2>&1; then
			work_owned=0
		else
			print -u2 "warning: could not remove the ephemeral worktree; it is left in place deliberately"
			cleanup_rc=1
		fi
	elif (( work_attempted )); then
		print -u2 "warning: a worktree path was reserved but never created; nothing was removed"
	fi
	if (( cleanup_rc == 0 )) && [[ -n "$work_parent" && -d "$work_parent" ]]; then
		rmdir -- "$work_parent" 2>/dev/null || true
	fi
	return $cleanup_rc
}

on_exit() { cleanup_worktree || true; }
on_signal() {
	trap - EXIT INT TERM HUP
	cleanup_worktree || true
	print -u2 "verify-pool-owning-root: terminated by signal"
	exit 143
}
trap on_exit EXIT
trap on_signal INT TERM HUP

events_valid() {
	local events=$1
	[[ -s "$events" ]] || return 1
	jq -e -s 'length > 0' "$events" >/dev/null 2>&1
}

# jq_predicate runs a COMPLETE jq expression. It is never piped into grep:
# under `set -o pipefail` a short-circuiting reader can SIGPIPE the writer and
# invert the result exactly when the predicate matched.
jq_predicate() {
	local events=$1
	local filter=$2
	jq -e -s "$filter" "$events" >/dev/null 2>&1
}

run_crashed() {
	local events=$1
	jq_predicate "$events" \
		'any(.[]; .Action == "output" and (.Output | test("^panic: |^fatal error: |test timed out after |\\[build failed\\]|^# ")))'
}

test_passed() {
	local events=$1
	local name=$2
	jq_predicate "$events" "any(.[]; .Action == \"pass\" and .Test == \"$name\")"
}

baseline_ok() {
	local events=$1
	local name
	events_valid "$events" || return 1
	run_crashed "$events" && return 1
	for name in ${=pool_expect}; do
		test_passed "$events" "$name" || return 1
	done
	return 0
}

killed_by() {
	local events=$1
	local name=$2
	local assertion=$3
	local exit_code=$4
	(( exit_code != 0 )) || return 1
	events_valid "$events" || return 1
	run_crashed "$events" && return 1
	jq_predicate "$events" "any(.[]; .Action == \"fail\" and .Test == \"$name\")" || return 1
	jq_predicate "$events" \
		"any(.[]; .Action == \"output\" and (.Test // \"\") == \"$name\" and (.Output | contains(\"$assertion\")))"
}

run_focused() {
	local selector=$1
	local events=$2
	local console=$3
	local pkg=${4:-$worktree_pkg}
	local rc=0
	( cd "$work" && timeout -k 10s "${wall_timeout}s" \
		go test -json -count=1 -p 1 -parallel 1 -timeout "${go_timeout}s" \
		-run "$selector" "$pkg" ) >"$events" 2>"$console" || rc=$?
	print -r -- "$rc"
}

compile_check() {
	local log=$1
	local pkg=${2:-$worktree_pkg}
	local rc=0
	( cd "$work" && timeout -k 10s "${compile_timeout}s" \
		go test -p 1 -c -o /dev/null "$pkg" ) >"$log" 2>&1 || rc=$?
	print -r -- "$rc"
}

typeset -A pristine

count_occurrences() {
	local haystack=$1
	local needle=$2
	local -a parts
	parts=("${(@ps:$needle:)haystack}")
	print -r -- $(( ${#parts} - 1 ))
}

# NOTE: `path` is a zsh special tied to PATH. A local named `path` here would
# replace command lookup with a filename, so source paths are always
# `source_path`.
patch_source() {
	local src=$1
	local from=$2
	local to=$3
	local source_path=$work/$src
	local content hits after
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to patch an unknown baseline"
		exit 1
	fi
	content=$(<"$source_path")
	hits=$(count_occurrences "$content" "$from")
	if [[ "$hits" != "1" ]]; then
		print -u2 "harness error: anchor appears $hits time(s) in $src; a control must patch exactly one site"
		exit 1
	fi
	print -r -- "${content//"$from"/"$to"}" >| "$source_path"
	after=$(git -C "$work" hash-object -- "$src")
	if [[ "$after" == "${pristine[$src]}" ]]; then
		print -u2 "harness error: the mutation did not change $src"
		exit 1
	fi
}

restore_source() {
	local src=$1
	local now
	if [[ -z "${pristine[$src]:-}" ]]; then
		print -u2 "harness error: no pristine hash for $src; refusing to restore against an unknown baseline"
		exit 1
	fi
	git -C "$work" checkout -- "$src"
	now=$(git -C "$work" hash-object -- "$src")
	if [[ "$now" != "${pristine[$src]}" ]]; then
		print -u2 "harness error: $src did not restore to its pristine content"
		exit 1
	fi
}

# ---------------------------------------------------------------------------
# The controls. Fields, separated by the record separator below:
#   name | source | package | anchor | replacement | killer | assertion
#
# The assertion is the FIRST one the killer emits under that mutant, derived
# from the mutated control flow, not from whatever the run happens to print.
# ---------------------------------------------------------------------------
sep=$'\x1f'
typeset -a controls
controls=(
"release-anchored-to-the-owning-repository${sep}${pool_src}${sep}${worktree_pkg}${sep}			base := p.DefaultBase
			if base == \"\" {
				base = \"origin/main\"
			}
			cmd := exec.CommandContext(ctx, \"git\", \"-C\", slotPath, \"reset\", \"--hard\", base)${sep}			base := p.DefaultBase
			if base == \"\" {
				base = \"origin/main\"
			}
			cmd := exec.CommandContext(ctx, \"git\", \"-C\", slot.Path, \"reset\", \"--hard\", base) // MUTANT: the stored path is resolved against the caller${sep}TestReleaseFromAForeignCallerResetsTheOwningSlot${sep}owning slot HEAD ="
"reclaim-anchored-to-the-owning-repository${sep}${pool_src}${sep}${worktree_pkg}${sep}		if out, err := exec.CommandContext(ctx, \"git\", \"-C\", slotPath, \"reset\", \"--hard\", base).CombinedOutput(); err != nil {${sep}		if out, err := exec.CommandContext(ctx, \"git\", \"-C\", slot.Path, \"reset\", \"--hard\", base).CombinedOutput(); err != nil { // MUTANT: unattended reclaim resolved against the caller${sep}TestReclaimFromAForeignCallerResetsTheOwningSlot${sep}reclaimed slot HEAD ="
"slot-path-contained-in-the-pool-root${sep}${pool_src}${sep}${worktree_pkg}${sep}	if err != nil || rel == \"..\" || strings.HasPrefix(rel, \"..\"+string(filepath.Separator)) || filepath.IsAbs(rel) {${sep}	if false && (err != nil || rel == \"..\" || strings.HasPrefix(rel, \"..\"+string(filepath.Separator)) || filepath.IsAbs(rel)) { // MUTANT: containment no longer refuses an escaping path${sep}TestReleaseRefusesAStoredPathOutsideThePoolRootAndKeepsTheLease${sep}containment did not protect the outside worktree"
"slot-must-be-a-registered-worktree${sep}${pool_src}${sep}${worktree_pkg}${sep}	if !registered {
		return \"\", fmt.Errorf(\"worktree pool: slot %s path %s is not a registered worktree of this repository; refusing to reset it\", slot.Name, resolved)${sep}	if false && !registered { // MUTANT: any contained directory counts as ours
		return \"\", fmt.Errorf(\"worktree pool: slot %s path %s is not a registered worktree of this repository; refusing to reset it\", slot.Name, resolved)${sep}TestReleaseRefusesAnUnregisteredPathAndKeepsTheLease${sep}the registration guard did not protect the foreign checkout"
"refused-release-keeps-the-lease${sep}${pool_src}${sep}${worktree_pkg}${sep}			// Anchored to the owning repository BEFORE anything destructive
			// runs, and refused rather than guessed. A failure here returns
			// with the lease still held: refusing to reset is always safer
			// than resetting a directory we have not proven is ours.
			slotPath, err := p.ownedSlotPath(ctx, *slot)
			if err != nil {
				return err
			}${sep}			slotPath, err := p.ownedSlotPath(ctx, *slot)
			if err != nil {
				slot.Purpose, slot.LeaseID = \"\", \"\" // MUTANT: a refused release frees the lease anyway
				slot.LeasedAt = time.Time{}
				_ = p.writeState(state)
				return err
			}${sep}TestReleaseKeepsTheLeaseWhenTheSlotIsGone${sep}a failed release cleared the lease"
"owning-roots-canonical-before-state-access${sep}${pool_src}${sep}${worktree_pkg}${sep}	abs, err := filepath.Abs(path)${sep}	abs, err := path, error(nil) // MUTANT: the owning roots keep the caller's spelling${sep}TestRelativeConstructorAnchorsBeforeTheCallerMoves${sep}the owning slot was not the one released"
"lease-incarnation-unique-per-assignment${sep}${pool_src}${sep}${worktree_pkg}${sep}	if n <= state.LastAssignedGeneration {${sep}	if false && n <= state.LastAssignedGeneration { // MUTANT: a fixed clock reissues a retired identity${sep}TestFixedClockMintsAUniqueIncarnationForEveryAssignment${sep}reissued the retired lease identity"
"legacy-state-generation-high-water${sep}${pool_src}${sep}${worktree_pkg}${sep}	state.LastAssignedGeneration = retainedGenerationHighWater(state)${sep}	// MUTANT: legacy state loads no derived high-water mark${sep}TestLegacyStateDoesNotReissueAReleasedGeneration${sep}is not above the retained"
"struct-literal-relative-roots-refused${sep}${pool_src}${sep}${worktree_pkg}${sep}		if !p.constructed && (!filepath.IsAbs(p.RepoRoot) || !filepath.IsAbs(p.Root)) {${sep}		if false && !p.constructed && (!filepath.IsAbs(p.RepoRoot) || !filepath.IsAbs(p.Root)) { // MUTANT: an unanchorable relative spelling reads the caller's state${sep}TestStructLiteralPoolRefusesRelativeRootsOnItsFirstStateRead${sep}read state instead of refusing"
)

# ---------------------------------------------------------------------------
# REGISTRY VALIDATION, before any baseline is spent: every control must have
# the exact field count, no empty field, a unique name, a source this driver
# owns, and an anchor that occurs EXACTLY ONCE in the LIVE source. A registry
# that has drifted from the source fails here rather than being reported as a
# survivor later.
# ---------------------------------------------------------------------------
typeset -A seen_names
typeset -A live_source
live_source[$pool_src]=$(<"$repo_root/$pool_src")

for row in "${controls[@]}"; do
	typeset -a fields
	fields=("${(@ps:$sep:)row}")
	if (( ${#fields} != 7 )); then
		print -u2 "harness error: control has ${#fields} field(s), want 7: ${fields[1]:-<unnamed>}"
		exit 1
	fi
	name=$fields[1]; src=$fields[2]; pkg=$fields[3]; anchor=$fields[4]
	replacement=$fields[5]; killer=$fields[6]; want=$fields[7]
	for label field in name "$name" source "$src" package "$pkg" anchor "$anchor" replacement "$replacement" killer "$killer" assertion "$want"; do
		if [[ -z "$field" ]]; then
			print -u2 "harness error: control ${name:-<unnamed>} has an empty $label"
			exit 1
		fi
	done
	if [[ -n "${seen_names[$name]:-}" ]]; then
		print -u2 "harness error: duplicate control name $name"
		exit 1
	fi
	seen_names[$name]=1
	if [[ -z "${live_source[$src]:-}" ]]; then
		print -u2 "harness error: control $name names a source this driver does not own: $src"
		exit 1
	fi
	hits=$(count_occurrences "${live_source[$src]}" "$anchor")
	if [[ "$hits" != "1" ]]; then
		print -u2 "harness error: control $name anchor occurs $hits time(s) in the live $src; it must occur exactly once"
		exit 1
	fi
	if [[ "$anchor" == "$replacement" ]]; then
		print -u2 "harness error: control $name replacement is identical to its anchor"
		exit 1
	fi
	if [[ "$pool_expect" != *"$killer"* ]]; then
		print -u2 "harness error: control $name names killer $killer, which the baseline does not run"
		exit 1
	fi
done
note "registry: ${#controls} control(s) validated against the live source before baseline"

pin=$(git -C "$repo_root" rev-parse HEAD)
work=$work_parent/checkout
work_attempted=1
if ! timeout -k 10s "${worktree_timeout}s" \
	git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1; then
	print -u2 "harness error: could not create the ephemeral worktree within ${worktree_timeout}s"
	exit 1
fi
work_owned=1

pristine[$pool_src]=$(git -C "$work" hash-object -- "$pool_src")
[[ -n "${pristine[$pool_src]}" ]] || { print -u2 "harness error: empty pristine hash for $pool_src"; exit 1; }

note "pin: $pin"
note "evidence: $run_dir"

# ---------------------------------------------------------------------------
# Baseline. Raw evidence is preserved whatever the outcome.
# ---------------------------------------------------------------------------
baseline_events=$run_dir/baseline.json
baseline_console=$run_dir/baseline.console
baseline_rc=$(run_focused "^(${pool_expect// /|})\$" "$baseline_events" "$baseline_console")
if (( baseline_rc != 0 )) || ! baseline_ok "$baseline_events"; then
	note "BASELINE FAILED (exit $baseline_rc) — see $baseline_events"
	exit 1
fi
note "baseline: all ${#${=pool_expect}} oracle(s) pass on unmutated source"

failures=0
for row in "${controls[@]}"; do
	typeset -a fields
	fields=("${(@ps:$sep:)row}")
	name=$fields[1]; src=$fields[2]; pkg=$fields[3]; anchor=$fields[4]
	replacement=$fields[5]; killer=$fields[6]; want=$fields[7]

	compile_log=$run_dir/$name.compile
	events=$run_dir/$name.json
	console=$run_dir/$name.console
	mutant_diff=$run_dir/$name.diff

	patch_source "$src" "$anchor" "$replacement"
	git -C "$work" diff -- "$src" >| "$mutant_diff" 2>/dev/null || true

	compile_rc=$(compile_check "$compile_log" "$pkg")
	if (( compile_rc != 0 )); then
		note "BROKEN-MUTANT $name: does not compile (see $compile_log)"
		restore_source "$src"
		failures=$(( failures + 1 ))
		continue
	fi

	rc=$(run_focused "^${killer}\$" "$events" "$console" "$pkg")
	restore_source "$src"

	if killed_by "$events" "$killer" "$want" "$rc"; then
		note "KILLED    $name -> $killer: \"$want\""
	elif (( rc == 0 )); then
		note "SURVIVED  $name: $killer passed with the guard removed (see $events)"
		failures=$(( failures + 1 ))
	elif run_crashed "$events"; then
		note "BROKEN-RUN $name: the run crashed rather than asserting (see $console)"
		failures=$(( failures + 1 ))
	elif ! jq_predicate "$events" "any(.[]; .Action == \"fail\" and .Test == \"$killer\")"; then
		note "WRONG-TEST $name: something other than $killer failed (see $events)"
		failures=$(( failures + 1 ))
	else
		note "WRONG-ASSERTION $name: $killer failed, but not on \"$want\" (see $events)"
		failures=$(( failures + 1 ))
	fi
done

restored=$(git -C "$work" hash-object -- "$pool_src")
print -r -- "$restored" >| "$run_dir/restored.hash"
if [[ "$restored" != "${pristine[$pool_src]}" ]]; then
	note "harness error: $pool_src did not end at its pristine content"
	failures=$(( failures + 1 ))
fi

cleanup_worktree || {
	note "harness error: the ephemeral worktree could not be removed"
	failures=$(( failures + 1 ))
}

if (( failures )); then
	note "verify-pool-owning-root: $failures control(s) did not prove their guard"
	exit 1
fi
note "verify-pool-owning-root: every control killed its named oracle on its named assertion"
