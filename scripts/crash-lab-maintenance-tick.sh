#!/usr/bin/env zsh
# Hosted Linux only. Disposable repo; no production data or live cleanup.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux maintenance-tick lab only"
  exit 1
fi

HERD="${HERD:?HERD binary required}"
chmod +x "$HERD"
WORKDIR="$(mktemp -d /tmp/herd-maint-lab.XXXXXX)"
LOG="${MAINT_LAB_LOG:-$WORKDIR/maint-lab.log}"
: >"$LOG"
holder=""
cleanup() {
  if [[ -n "${holder:-}" ]]; then
    kill "$holder" 2>/dev/null || true
    wait "$holder" 2>/dev/null || true
    holder=""
  fi
  if [[ -n "${LOG:-}" && -f "$LOG" && "$LOG" != "$WORKDIR"/* ]]; then
    :
  fi
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

unset HERD_ROOT HERD_REPO_ROOT HERD_PROJECT_ROOT HERD_CONFIG_PATH HERD_WORKSPACE HERD_LANE || true
export HERD_PROJECT_ROOT="$WORKDIR"

log() { print -r -- "$*" | tee -a "$LOG"; }

git -C "$WORKDIR" init -q
git -C "$WORKDIR" config user.email lab@example.com
git -C "$WORKDIR" config user.name lab
print -r -- 'ok' >"$WORKDIR/f"
git -C "$WORKDIR" add f
git -C "$WORKDIR" commit -qm init
git -C "$WORKDIR" branch -M main

integer i
for i in {1..9}; do
  git -C "$WORKDIR" worktree add -q -b "leftover-$i" "$WORKDIR/wt-$i" main
done
git -C "$WORKDIR" checkout -q main

mkdir -p "$WORKDIR/.herd"
cat >"$WORKDIR/.herd/herd.yaml" <<'YAML'
project:
  name: maint-lab
  default_branch: main
YAML

LOCK="$WORKDIR/.herd/worktree-reap-pulse.lock"
READY="$WORKDIR/lock.ready"
mkdir -p "$WORKDIR/.herd"
touch "$LOCK"
(
  exec 9>"$LOCK"
  flock -n 9 || exit 1
  print -r -- ready >"$READY"
  sleep 30
) &
holder=$!
integer n=0
while (( n < 50 )); do
  [[ -f "$READY" ]] && break
  sleep 0.1
  n=$((n + 1))
done
[[ -f "$READY" ]] || { print -u2 "flock holder never became ready"; exit 1; }

set +e
out="$(cd "$WORKDIR/wt-1" && "$HERD" maintenance --act 2>&1)"
rc=$?
set -e
log "$out"
print -r -- "$out" | grep -E -q 'deferred|tick lock' || {
  print -u2 "missing deferred/tick lock while flock held (rc=$rc): $out"
  exit 1
}
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true
holder=""

cursor="$WORKDIR/.herd/worktree-reap-pulse.cursor"
out1="$(cd "$WORKDIR/wt-1" && "$HERD" maintenance --act 2>&1)"
log "$out1"
print -r -- "$out1" | grep -E -q 'inspected=[1-9]' || {
  print -u2 "wt-1 inspected not >0: $out1"
  exit 1
}
[[ -f "$cursor" ]] || { print -u2 "missing shared cursor after wt-1"; exit 1; }
[[ ! -e "$WORKDIR/wt-1/.herd/worktree-reap-pulse.cursor" ]] || { print -u2 "wt-1 grew its own cursor"; exit 1; }
last1="$(cat "$cursor")"

out2="$(cd "$WORKDIR/wt-2" && "$HERD" maintenance --act 2>&1)"
log "$out2"
print -r -- "$out2" | grep -E -q 'inspected=[1-9]' || {
  print -u2 "wt-2 inspected not >0: $out2"
  exit 1
}
[[ -f "$cursor" ]] || { print -u2 "shared cursor vanished"; exit 1; }
[[ ! -e "$WORKDIR/wt-2/.herd/worktree-reap-pulse.cursor" ]] || { print -u2 "wt-2 grew its own cursor"; exit 1; }
last2="$(cat "$cursor")"
[[ "$last1" != "$last2" ]] || { print -u2 "cursor last did not advance: $last1"; exit 1; }

log "maintenance shared tick lab ok last1=$last1 last2=$last2"
