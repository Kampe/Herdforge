#!/usr/bin/env zsh
# FAC-828 non-vacuity control for the bounded inbox.
#
# A bound that was never shown to fail is not a bound. Each mutant must
# COMPILE and then die on its NAMED assertion: a compile error proves nothing
# and is refused as a kill. Baseline must pass before any mutant means
# anything, and the source is restored and re-proved after every one.
set -euo pipefail

repo_root=${0:A:h:h}
cd -- "$repo_root"

source_rel=pkg/mail/bounded.go
cli_rel=cmd/herd/mail_bounds.go
test_run='TestBoundedInbox'
pkg=./cmd/herd/
go_timeout=${VERIFY_BOUNDS_TIMEOUT:-600}

report_parent=${VERIFY_BOUNDS_REPORT_DIR:-$repo_root/.verify-bounds-logs}
mkdir -p -- "$report_parent"
run_dir=$(mktemp -d "$report_parent/run-XXXXXX")

note() { print -r -- "$@" }

pristine_source=$(git hash-object -- "$source_rel")
pristine_cli=$(git hash-object -- "$cli_rel")

restore() {
	git checkout --quiet -- "$source_rel" "$cli_rel"
	local a b
	a=$(git hash-object -- "$source_rel"); b=$(git hash-object -- "$cli_rel")
	[[ "$a" == "$pristine_source" && "$b" == "$pristine_cli" ]] \
		|| { print -u2 "error: restore left $a/$b, want $pristine_source/$pristine_cli"; return 1 }
}
trap 'restore || true' EXIT

# patch replaces exactly one whole-line anchor. The guard compares OCCURRENCES,
# not matching lines: grep -c would report 1 for a line holding the anchor
# twice and the global substitution below would then mutate both.
patch() {
	local file=$1 anchor=$2 replacement=$3 found content
	found=$(grep -F -o -- "$anchor" "$file" | wc -l | tr -d ' ')
	if [[ "$found" != 1 ]]; then
		print -u2 "error: anchor matched ${found:-0} occurrences, want exactly 1 (source drifted): $anchor"
		return 1
	fi
	content=$(<"$file")
	print -r -- "${content//"$anchor"/"$replacement"}" >| "$file"
}

run_tests() {
	local out=$1 rc=0
	go test -count=1 -run "$test_run" -timeout "${go_timeout}s" "$pkg" >|"$out" 2>&1 || rc=$?
	print -r -- "$rc"
}

compile_check() {
	local out=$1 rc=0
	go vet "$pkg" ./pkg/mail/ >|"$out" 2>&1 || rc=$?
	print -r -- "$rc"
}

baseline=$(run_tests "$run_dir/baseline.log")
if [[ "$baseline" != 0 ]]; then
	note "baseline FAILED (exit $baseline) - nothing below can mean anything"
	exit 1
fi
note 'baseline PASS'

sep=$'\x1f'
# id | file | anchor | replacement | killer test
mutations=(
"limit-not-enforced${sep}${source_rel}${sep}		if len(page.Envelopes) >= opts.Limit || page.Bytes+len(line) > opts.MaxBytes {${sep}		if false { // MUTANT: limit and byte budget ignored${sep}TestBoundedInboxLimitBindsAndCursorAdvances"
"cursor-recipient-unbound${sep}${source_rel}${sep}	if want := encodeCursorRecipient(recipient); parts[1] != want {${sep}	if false { // MUTANT: cursor recipient binding dropped${sep}TestBoundedInboxRejectsUnusableCursors"
"cursor-version-unchecked${sep}${source_rel}${sep}	if parts[0] != cursorVersion {${sep}	if false { // MUTANT: cursor version accepted blindly${sep}TestBoundedInboxRejectsUnusableCursors"
"control-mark-shared-with-feedback${sep}${source_rel}${sep}		if env.Sequence <= cur.Control {${sep}		if env.Sequence <= cur.Feedback { // MUTANT: one shared mark across two sequence spaces${sep}TestBoundedInboxKeepsIndependentSequenceSpacesWhole"
"cancellation-swallowed${sep}${source_rel}${sep}		if err := ctx.Err(); err != nil {
			return page, err
		}${sep}		_ = ctx${sep}TestBoundedInboxPropagatesCancellation"
"feedback-error-swallowed${sep}${cli_rel}${sep}			return nil, highest, false, fmt.Errorf(\"mail: unparseable feedback record in feedback store: %w\", err)${sep}			continue // MUTANT: unreadable feedback silently dropped${sep}TestBoundedInboxFailsOnUnreadableFeedbackRecord"
)

failures=0
index=0
for record in "${mutations[@]}"; do
	(( index += 1 ))
	fields=("${(@ps:$sep:)record}")
	id=$fields[1]; file=$fields[2]; anchor=$fields[3]; replacement=$fields[4]; killer=$fields[5]
	stem=$run_dir/$(printf 'm%02d-%s' "$index" "$id")

	patch "$file" "$anchor" "$replacement"

	compile_exit=$(compile_check "$stem.compile.log")
	if [[ "$compile_exit" != 0 ]]; then
		note "$id: DID NOT COMPILE - refused as a kill (a compile error proves nothing)"
		(( failures += 1 ))
		restore
		continue
	fi

	mutant_exit=$(run_tests "$stem.log")
	if [[ "$mutant_exit" == 0 ]]; then
		note "$id: SURVIVED - $killer did not fail"
		(( failures += 1 ))
	elif grep -q -- "--- FAIL: $killer" "$stem.log"; then
		note "$id: KILLED by $killer"
	else
		note "$id: WRONG-TEST - exited $mutant_exit without $killer failing"
		(( failures += 1 ))
	fi

	restore
	restored_exit=$(run_tests "$stem.restored.log")
	if [[ "$restored_exit" != 0 ]]; then
		note "$id: restore did not return to PASS (exit $restored_exit)"
		(( failures += 1 ))
	fi
done

note "mutants=$index failures=$failures"
(( failures == 0 )) || exit 1
note 'all bounded-inbox controls killed their intended assertions'
