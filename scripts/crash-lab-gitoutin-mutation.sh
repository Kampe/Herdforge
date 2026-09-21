#!/usr/bin/env zsh
# Hosted Linux only. Mixing stderr into stdout must fail the warning-vs-dirty test.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi
FILE=cmd/herd/worktreereap.go
FROM='cmd.Stderr = &stderr'
TO='cmd.Stderr = &stdout'
restore() { git checkout -- "$FILE"; }
if ! git diff --quiet -- "$FILE" || ! git diff --cached --quiet -- "$FILE"; then
  print -u2 "refuse dirty $FILE"
  exit 1
fi
trap restore EXIT
n="$(grep -F -c -- "$FROM" "$FILE" || true)"
if [[ "$n" != "1" ]]; then
  print -u2 "literal not unique count=$n"
  exit 1
fi
print -r -- "BASELINE"
set +e
base="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestGitOutInStatusIgnoresStderrWarningsWhenStdoutClean$' 2>&1)"
brc=$?
set -e
print -r -- "$base"
if (( brc != 0 )); then
  print -u2 "baseline failed; not mutating"
  exit 1
fi
FROM="$FROM" TO="$TO" perl -i -pe 'BEGIN { $from = $ENV{FROM}; $to = $ENV{TO} } s/\Q$from\E/$to/' "$FILE"
set +e
out="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestGitOutInStatusIgnoresStderrWarningsWhenStdoutClean$' 2>&1)"
rc=$?
set -e
print -r -- "$out"
if (( rc == 0 )); then
  print -u2 "stderr-into-stdout mutant did not fail"
  exit 1
fi
print -r -- "$out" | grep -F -q 'FAIL: TestGitOutInStatusIgnoresStderrWarningsWhenStdoutClean' || {
  print -u2 "not named FAIL"
  exit 1
}
print -r -- "$out" | grep -F -q 'rc0 stderr warnings must not classify a clean tree dirty' || {
  print -u2 "missing dirty-from-warning assertion"
  exit 1
}
restore
set +e
post="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestGitOutInStatusIgnoresStderrWarningsWhenStdoutClean$' 2>&1)"
prc=$?
set -e
print -r -- "$post"
if (( prc != 0 )); then
  print -u2 "restored test failed"
  exit 1
fi
print -r -- "gitOutIn mutation driver ok"
