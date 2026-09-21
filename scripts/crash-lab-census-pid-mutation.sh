#!/usr/bin/env zsh
# Hosted Linux only. Recording nonmatching PIDs must fail the unique-PID test.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi
FILE=pkg/resources/git_census.go
FROM=$'if !referenced {\n\t\t\tcontinue\n\t\t}'
TO=$'if !referenced && false {\n\t\t\tcontinue\n\t\t}'
restore() { git checkout -- "$FILE"; }
if ! git diff --quiet -- "$FILE" || ! git diff --cached --quiet -- "$FILE"; then
  print -u2 "refuse dirty $FILE"
  exit 1
fi
trap restore EXIT
n="$(grep -F -c -- $'if !referenced {\n\t\t\tcontinue' "$FILE" || true)"
if [[ "$n" != "1" ]]; then
  print -u2 "if !referenced not unique count=$n"
  exit 1
fi
print -r -- "BASELINE"
set +e
base="$(go test -count=1 -timeout=60s ./pkg/resources -run '^TestInUseManyWalkRecordsOnlyMatchingUniquePIDs$|^TestAppendUniquePIDMatchingNonmatchingAndDuplicate$' 2>&1)"
brc=$?
set -e
print -r -- "$base"
if (( brc != 0 )); then
  print -u2 "baseline PID tests failed; not mutating"
  exit 1
fi
perl -i -0pe 's/if !referenced \{\n\t\t\tcontinue\n\t\t\}/if !referenced \&\& false {\n\t\t\tcontinue\n\t\t}/' "$FILE"
set +e
out="$(go test -count=1 -timeout=60s ./pkg/resources -run '^TestInUseManyWalkRecordsOnlyMatchingUniquePIDs$' 2>&1)"
rc=$?
set -e
print -r -- "$out"
if (( rc == 0 )); then
  print -u2 "nonmatching-PID mutant did not fail"
  exit 1
fi
print -r -- "$out" | grep -F -q 'FAIL: TestInUseManyWalkRecordsOnlyMatchingUniquePIDs' || {
  print -u2 "not named FAIL"
  exit 1
}
print -r -- "$out" | grep -F -q 'matching unique pids' || {
  print -u2 "missing first matching-unique assertion (pid8 also appends to matching path)"
  exit 1
}
restore
set +e
post="$(go test -count=1 -timeout=60s ./pkg/resources -run '^TestInUseManyWalkRecordsOnlyMatchingUniquePIDs$|^TestAppendUniquePIDMatchingNonmatchingAndDuplicate$' 2>&1)"
prc=$?
set -e
print -r -- "$post"
if (( prc != 0 )); then
  print -u2 "restored PID tests failed"
  exit 1
fi
print -r -- "census PID mutation driver ok"
