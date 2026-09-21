#!/usr/bin/env zsh
# Hosted Linux only. Recording every walk pid in InUse must fail the single-path test.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi
FILE=pkg/resources/git_census.go
FROM='			usage.PIDs = appendUniquePID(usage.PIDs, pid)'
TO='			usage.PIDs = appendUniquePID(usage.PIDs, pid); usage.PIDs = appendUniquePID(usage.PIDs, pid+1-pid)'
# pid+1-pid is pid, duplicate no-op... need to append all pids
# Better: if referenced || true around the InUse block.
restore() { git checkout -- "$FILE"; }
if ! git diff --quiet -- "$FILE" || ! git diff --cached --quiet -- "$FILE"; then
  print -u2 "refuse dirty $FILE"
  exit 1
fi
trap restore EXIT
n="$(grep -F -c -- 'if referenced {' "$FILE" || true)"
if [[ "$n" != "1" ]]; then
  print -u2 "if referenced { not unique count=$n"
  exit 1
fi
print -r -- "BASELINE InUse PID test"
set +e
base="$(go test -count=1 -timeout=60s ./pkg/resources -run '^TestInUseRecordsMatchingNonmatchingAndLsofDuplicate$' 2>&1)"
brc=$?
set -e
print -r -- "$base"
if (( brc != 0 )); then
  print -u2 "baseline InUse PID test failed; not mutating"
  exit 1
fi
perl -i -pe 'BEGIN { $n = 0 } $n += s/\Qif referenced {\E/if referenced || true {/; END { exit($n == 1 ? 0 : 2) }' "$FILE" || {
  print -u2 "perl InUse substitution count not exactly 1"
  exit 1
}
if git diff --quiet -- "$FILE"; then
  print -u2 "InUse mutation was a no-op; refusing unchanged source"
  exit 1
fi
set +e
out="$(go test -count=1 -timeout=60s ./pkg/resources -run '^TestInUseRecordsMatchingNonmatchingAndLsofDuplicate$' 2>&1)"
rc=$?
set -e
print -r -- "$out"
if (( rc == 0 )); then
  print -u2 "InUse record-all mutant did not fail"
  exit 1
fi
print -r -- "$out" | grep -F -q 'FAIL: TestInUseRecordsMatchingNonmatchingAndLsofDuplicate' || {
  print -u2 "not named FAIL"
  exit 1
}
print -r -- "$out" | grep -F -q 'single InUse pids' || {
  print -u2 "missing single InUse pids assertion"
  exit 1
}
restore
set +e
post="$(go test -count=1 -timeout=60s ./pkg/resources -run '^TestInUseRecordsMatchingNonmatchingAndLsofDuplicate$' 2>&1)"
prc=$?
set -e
print -r -- "$post"
if (( prc != 0 )); then
  print -u2 "restored InUse PID test failed"
  exit 1
fi
print -r -- "InUse PID mutation driver ok"
