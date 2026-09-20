#!/usr/bin/env zsh
# Exercise the SAME suite runner used by both landed-control drivers. Only the
# expensive Go execution is substituted with distinguishable fixture output;
# filenames, manifest records, redirections and retained-file reads are real.
set -euo pipefail

(( $# == 2 )) || { print -u2 'usage: verify-landed-suite-retention.zsh LIBRARY NEW_REPORT_DIR'; exit 2; }
library=${1:A}
report=${2:A}
[[ -f "$library" ]] || { print -u2 'suite runner library is missing'; exit 2; }
# The caller allocates a unique run directory. Never overwrite an older proof.
mkdir -- "$report"
cd "$report"
pristine=$(git hash-object -- "$library")
cp -- "$library" runner.zsh

restore_runner() {
	cp -- "$library" runner.zsh || return 1
	[[ "$(git hash-object -- "$library")" == "$pristine" && \
	   "$(git hash-object -- runner.zsh)" == "$pristine" ]]
}

cleanup() {
	local code=$?
	trap - EXIT
	if ! restore_runner; then
		print -u2 'suite runner restoration FAILED'
		code=1
	fi
	exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

run_case() (
	local case_name=$1
	run_dir="$report/$case_name"
	mkdir -- "$run_dir"
	summary="$run_dir/summary.txt"
	sep=$'\x1f'
	suites=(
		"./pkg/mergeadmit/${sep}^TestSuiteAlpha\$${sep}TestSuiteAlpha"
		"./pkg/mergeadmit/${sep}^TestSuiteBeta\$${sep}TestSuiteBeta"
	)
	note() { print -r -- "$1" >> "$summary"; }
	run_focused() {
		local pkg=$1 selector=$2 events=$3 console=$4 name
		name=${selector#\^}
		name=${name%\$}
		jq -cn --arg name "$name" --arg pkg "$pkg" \
			'{Action:"output", Package:$pkg, Test:$name, Output:("fixture stdout: " + $name + "\n")},
			 {Action:"pass", Package:$pkg, Test:$name}' > "$events" || return 1
		print -r -- "fixture stderr: $name" > "$console"
		print -r -- 0
	}
	assert_required_identities() {
		jq -es --arg name "$3" 'any(.[]; .Action == "pass" and .Test == $name)' "$1" >/dev/null
	}
	source "$report/runner.zsh"
	run_all_suites baseline || return 1
	run_all_suites restored || return 1
)

retention_oracle() {
	local directory=$1 row events console required
	# Establish that BOTH rows actually ran in BOTH phases before judging
	# retention. A broken runner or malformed JSON is not a causal kill.
	if ! jq -es '
		length == 4 and
		([.[] | [.phase,.ordinal]] | sort) == [["baseline",1],["baseline",2],["restored",1],["restored",2]] and
		all(.[]; .package == "./pkg/mergeadmit/" and
		  .required == (if .ordinal == 1 then "TestSuiteAlpha" else "TestSuiteBeta" end) and
		  .selector == ("^" + .required + "$") and
		  (.events | test("^[a-zA-Z0-9-]+\\.json$")) and
		  (.stderr | test("^[a-zA-Z0-9-]+\\.err$")))
	' "$directory/suites.jsonl" >/dev/null; then
		print -u2 'fixture suite executions did not retain valid identities'
		return 2
	fi
	if ! jq -es '([.[].events] | unique | length) == 4 and
		([.[].stderr] | unique | length) == 4' "$directory/suites.jsonl" >/dev/null; then
		print -u2 'ASSERTION: suite retention lost distinct same-package transcripts'
		return 1
	fi
	while IFS= read -r row; do
		events=$(jq -r '.events' <<< "$row")
		console=$(jq -r '.stderr' <<< "$row")
		required=$(jq -r '.required' <<< "$row")
		if [[ ! -f "$directory/$events" || ! -f "$directory/$console" ]] || \
			! jq -es --arg name "$required" \
			'([.[] | select(.Action == "pass") | .Test] == [$name]) and
			 any(.[]; .Action == "output" and .Test == $name and .Output == ("fixture stdout: " + $name + "\n"))' \
			"$directory/$events" >/dev/null; then
			print -u2 'ASSERTION: suite retention lost transcript identity or bytes'
			return 1
		fi
		if [[ "$(<"$directory/$console")" != "fixture stderr: $required" ]]; then
			print -u2 'ASSERTION: suite retention lost stderr identity or bytes'
			return 1
		fi
	done < "$directory/suites.jsonl"
}

run_case baseline > baseline-run.log 2>&1
retention_oracle baseline > baseline-oracle.log 2>&1
print -r -- 'baseline retention PASS'

anchor=$'\t\tstem="$label-s${suite_index}-$slug"'
replacement=$'\t\tstem="$label-$slug"'
body=$(<runner.zsh)
[[ "$body" == *"$anchor"* && "${body#*"$anchor"}" != *"$anchor"* ]] || {
	print -u2 'expected one suite naming anchor'; exit 2
}
print -r -- "${body/"$anchor"/"$replacement"}" > runner.zsh
[[ "$(git hash-object -- runner.zsh)" != "$pristine" ]] || { print -u2 'naming mutant did not change source'; exit 2; }
zsh -n runner.zsh > mutant-syntax.log 2>&1
# A script-valid mutant must complete the runner before the retention oracle
# rejects it. Syntax, fixture execution and assertion evidence stay separate.
run_case mutant > mutant-run.log 2>&1
mutant_exit=0
retention_oracle mutant > mutant-oracle.log 2>&1 || mutant_exit=$?
if (( mutant_exit != 1 )) || [[ "$(<mutant-oracle.log)" != 'ASSERTION: suite retention lost distinct same-package transcripts' ]]; then
	print -u2 'package-only naming mutant did not fail the exact retention assertion'
	exit 1
fi
print -r -- 'package-only naming mutant: syntax PASS, runner PASS, retention KILLED'

restore_runner
run_case restored > restored-run.log 2>&1
retention_oracle restored > restored-oracle.log 2>&1
print -r -- 'restored retention PASS; runner bytes match pristine source'
