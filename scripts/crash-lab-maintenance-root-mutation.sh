#!/usr/bin/env zsh
# Hosted Linux only. Mutate cleanupCoordinationRoot back to per-worktree cwd.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi

FILE=cmd/herd/worktreereap_pulse.go
FROM='root, _, err := gitroot.ProjectRoot(ctx, start)'
TO='return canonicalRepoRoot(start), nil'
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

print -r -- "BASELINE linked-root PASS"
go test -count=1 -timeout=60s ./cmd/herd -run '^TestMaintenanceLinkedWorktreeSharesProjectRoot$'

FROM="$FROM" TO="$TO" perl -i -pe 'BEGIN { $from = $ENV{FROM}; $to = $ENV{TO} } s/\Q$from\E/$to/' "$FILE"

set +e
out="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestMaintenanceLinkedWorktreeSharesProjectRoot$' 2>&1)"
rc=$?
set -e
print -r -- "$out"
if (( rc == 0 )); then
  print -u2 "per-worktree regression did not fail linked-root test"
  exit 1
fi
print -r -- "$out" | grep -F -q 'FAIL: TestMaintenanceLinkedWorktreeSharesProjectRoot' || {
  print -u2 "not a named FAIL"
  exit 1
}
print -r -- "$out" | grep -F -q 'want shared project root' || {
  print -u2 "missing assertion want shared project root"
  exit 1
}

restore
if ! git diff --quiet -- "$FILE"; then
  print -u2 "restore left dirty $FILE"
  exit 1
fi
print -r -- "POST-MUTANT linked-root PASS"
go test -count=1 -timeout=60s ./cmd/herd -run '^TestMaintenanceLinkedWorktreeSharesProjectRoot$|^TestCleanupCoordinationRootRefusesCanceledContext$'
print -r -- "maintenance root mutation driver ok"
