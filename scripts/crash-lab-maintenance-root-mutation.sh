#!/usr/bin/env zsh
# Hosted Linux only. Mutate cleanupCoordinationRoot back to per-worktree cwd.
set -euo pipefail
if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi

FILE=cmd/herd/worktreereap_pulse.go
FROM='root, _, err := gitroot.ProjectRoot(ctx, start)'
TO='root, err := canonicalRepoRoot(start), error(nil)'
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

LOG="${MUTATION_LOG:-maintenance-root-mutation.log}"
: >"$LOG"
log() { print -r -- "$*" | tee -a "$LOG"; }
log "BASELINE linked-root"
set +e
base="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestMaintenanceLinkedWorktreeSharesProjectRoot$' 2>&1)"
base_rc=$?
set -e
print -r -- "$base" | tee -a "$LOG"
if (( base_rc != 0 )); then
  print -u2 "baseline linked-root failed; not mutating"
  exit 1
fi
log "BASELINE linked-root PASS"

FROM="$FROM" TO="$TO" perl -i -pe 'BEGIN { $from = $ENV{FROM}; $to = $ENV{TO} } s/\Q$from\E/$to/' "$FILE"

set +e
out="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestMaintenanceLinkedWorktreeSharesProjectRoot$' 2>&1)"
rc=$?
set -e
print -r -- "$out" | tee -a "$LOG"
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
log "POST-MUTANT"
set +e
post="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestMaintenanceLinkedWorktreeSharesProjectRoot$|^TestCleanupCoordinationRootRefusesCanceledContext$' 2>&1)"
post_rc=$?
set -e
print -r -- "$post" | tee -a "$LOG"
if (( post_rc != 0 )); then
  print -u2 "post-mutant restore tests failed"
  exit 1
fi
log "POST-MUTANT linked-root PASS"

log "DANGLING-LEAF MUTANT"
DFROM='if info.Mode()&os.ModeSymlink != 0 {'
DTO='if false {'
n="$(grep -F -c -- "$DFROM" "$FILE" || true)"
if [[ "$n" != "1" ]]; then
  print -u2 "dangling literal not unique count=$n"
  exit 1
fi
FROM="$DFROM" TO="$DTO" perl -i -pe 'BEGIN { $from = $ENV{FROM}; $to = $ENV{TO} } s/\Q$from\E/$to/' "$FILE"
set +e
dout="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestCleanupCoordinationRootDanglingLeaf$' 2>&1)"
drc=$?
set -e
print -r -- "$dout" | tee -a "$LOG"
if (( drc == 0 )); then
  print -u2 "removing symlink-ancestor refusal did not fail dangling-leaf test"
  exit 1
fi
print -r -- "$dout" | grep -F -q 'dangling leaf must refuse' || {
  print -u2 "missing dangling leaf assertion"
  exit 1
}
restore
if ! git diff --quiet -- "$FILE"; then
  print -u2 "dangling mutant restore left dirty $FILE"
  exit 1
fi
set +e
dpost="$(go test -count=1 -timeout=60s ./cmd/herd -run '^TestCleanupCoordinationRootDanglingLeaf$' 2>&1)"
dpost_rc=$?
set -e
print -r -- "$dpost" | tee -a "$LOG"
if (( dpost_rc != 0 )); then
  print -u2 "dangling-leaf test failed after restore"
  exit 1
fi
log "maintenance root mutation driver ok"
