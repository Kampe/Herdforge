#!/usr/bin/env zsh
# Hosted Linux only. Entire fixture runs as one unprivileged UID.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux maintenance-tick lab only"
  exit 1
fi

HERD="${HERD:?HERD binary required}"
chmod +x "$HERD"
command -v lsof >/dev/null || { print -u2 "lsof required for owner census"; exit 1; }
WORKDIR="$(mktemp -d /tmp/herd-maint-lab.XXXXXX)"
LOG="${MAINT_LAB_LOG:-$WORKDIR/maint-lab.log}"
: >"$LOG"
holder=""
owned_pid=""
cleanup() {
  local dirty=0
  if [[ -n "${owned_pid:-}" ]]; then
    kill "$owned_pid" 2>/dev/null || dirty=1
    wait "$owned_pid" 2>/dev/null || true
    if kill -0 "$owned_pid" 2>/dev/null; then
      kill -KILL "$owned_pid" 2>/dev/null || true
      wait "$owned_pid" 2>/dev/null || true
    fi
    if kill -0 "$owned_pid" 2>/dev/null; then
      dirty=1
    fi
    owned_pid=""
  fi
  if [[ -n "${holder:-}" ]]; then
    kill "$holder" 2>/dev/null || dirty=1
    wait "$holder" 2>/dev/null || true
    holder=""
  fi
  rm -rf "$WORKDIR" || dirty=1
  if (( dirty )); then
    exit 1
  fi
}
trap cleanup EXIT

unset HERD_ROOT HERD_REPO_ROOT HERD_PROJECT_ROOT HERD_CONFIG_PATH HERD_WORKSPACE HERD_LANE || true

log() { print -r -- "$*" | tee -a "$LOG"; }

git -C "$WORKDIR" init -q
git -C "$WORKDIR" config user.email lab@example.com
git -C "$WORKDIR" config user.name lab
print -r -- 'ok' >"$WORKDIR/f"
git -C "$WORKDIR" add f
git -C "$WORKDIR" commit -qm init
git -C "$WORKDIR" branch -M main
git -C "$WORKDIR" update-ref refs/remotes/origin/main main

integer i
for i in {1..12}; do
  git -C "$WORKDIR" worktree add -q -b "leftover-$i" "$WORKDIR/wt-$i" main
  print -r -- "unmerged-$i" >"$WORKDIR/wt-$i/extra-$i"
  git -C "$WORKDIR/wt-$i" add "extra-$i"
  git -C "$WORKDIR/wt-$i" commit -qm "unmerged leftover-$i"
done
git -C "$WORKDIR" checkout -q main

MERGED="$WORKDIR/wt-00-merged"
DIRTY="$WORKDIR/wt-01-dirty"
OWNED="$WORKDIR/wt-02-owned"
git -C "$WORKDIR" worktree add -q -b merged-ok "$MERGED" origin/main
git -C "$WORKDIR" worktree add -q -b dirty-ok "$DIRTY" origin/main
print -r -- 'dirt' >"$DIRTY/dirt"
git -C "$WORKDIR" worktree add -q -b owned-ok "$OWNED" origin/main
(
  cd "$OWNED" || exit 1
  print -r -- ready >"$WORKDIR/owned.ready"
  exec sleep 120
) &
owned_pid=$!
integer on=0
while (( on < 50 )); do
  [[ -f "$WORKDIR/owned.ready" ]] && break
  sleep 0.1
  on=$((on + 1))
done
[[ -f "$WORKDIR/owned.ready" ]] || { print -u2 "owned sleep never ready"; exit 1; }
kill -0 "$owned_pid" || { print -u2 "owned sleep pid $owned_pid not live"; exit 1; }
cwd="$(readlink "/proc/$owned_pid/cwd" 2>/dev/null || true)"
[[ "$cwd" == "$OWNED" ]] || { print -u2 "owned cwd $cwd want $OWNED"; exit 1; }

INVOKING="$WORKDIR/wt-1"
window="$(
  git -C "$WORKDIR" worktree list --porcelain | awk '/^worktree /{print $2}' | while read -r p; do
    [[ -n "$p" ]] || continue
    [[ "$p" == "$WORKDIR" || "$p" == "$INVOKING" ]] && continue
    print -r -- "$p"
  done | sort | head -8
)"
print -r -- "$window" | grep -F -x "$MERGED" >/dev/null || {
  print -u2 "merged fixture not in first inspect window:"
  print -r -- "$window"
  exit 1
}
print -r -- "$window" | grep -F -x "$DIRTY" >/dev/null || {
  print -u2 "dirty merged fixture not in first inspect window:"
  print -r -- "$window"
  exit 1
}
print -r -- "$window" | grep -F -x "$OWNED" >/dev/null || {
  print -u2 "owned merged fixture not in first inspect window:"
  print -r -- "$window"
  exit 1
}

mkdir -p "$WORKDIR/.herd"
cat >"$WORKDIR/.herd/herd.yaml" <<'YAML'
project:
  name: maint-lab
  default_branch: main
YAML

LOCK="$WORKDIR/.herd/worktree-reap-pulse.lock"
READY="$WORKDIR/lock.ready"
touch "$LOCK"
(
  exec 9>"$LOCK"
  flock -n 9 || exit 1
  print -r -- ready >"$READY"
  exec sleep 30
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
(( rc == 0 )) || { print -u2 "deferred maintenance rc=$rc want 0: $out"; exit 1; }
print -r -- "$out" | grep -E -q 'deferred|tick lock' || {
  print -u2 "missing deferred/tick lock while flock held: $out"
  exit 1
}
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true
holder=""

cursor="$WORKDIR/.herd/worktree-reap-pulse.cursor"
log "pre-act ahead/status merged=$(git -C "$MERGED" rev-list --count origin/main..HEAD 2>&1) status=$(git -C "$MERGED" status --porcelain=v1 2>&1)"
log "pre-act ahead/status owned=$(git -C "$OWNED" rev-list --count origin/main..HEAD 2>&1) status=$(git -C "$OWNED" status --porcelain=v1 2>&1)"
set +e
out1="$(cd "$WORKDIR/wt-1" && "$HERD" maintenance --act 2>&1)"
rc1=$?
set -e
log "$out1"
print -r -- "$out1" | grep -E -q 'eligible=(9|[1-9][0-9]+)' || {
  print -u2 "wt-1 eligible not >8: $out1"
  exit 1
}
print -r -- "$out1" | grep -E -q 'inspected=8([^0-9]|$)' || {
  print -u2 "wt-1 inspected not 8: $out1"
  exit 1
}
print -r -- "$out1" | grep -E -q 'landed=[1-9]' || {
  print -u2 "expected landed>=1: $out1"
  exit 1
}
(( rc1 != 0 )) || { print -u2 "maintenance must exit nonzero when Failed>0; rc=$rc1 out=$out1"; exit 1; }
print -r -- "$out1" | grep -E -q 'failed=[1-9]' || {
  print -u2 "expected owner-census failed>=1 (rc=$rc1): $out1"
  exit 1
}
print -r -- "$out1" | grep -F -q "$OWNED" || {
  print -u2 "refusal missing exact owned path $OWNED: $out1"
  exit 1
}
print -r -- "$out1" | grep -E -q 'owner census found active use' || {
  print -u2 "missing native owner-census refusal: $out1"
  exit 1
}
print -r -- "$out1" | grep -E -q 'retired=1([^0-9]|$)' || {
  print -u2 "expected retired=1 for merged fixture: $out1"
  exit 1
}
[[ ! -e "$MERGED" ]] || { print -u2 "merged fixture still on disk"; exit 1; }
git -C "$WORKDIR" worktree list --porcelain | grep -F -q "$MERGED" && { print -u2 "merged fixture still registered"; exit 1; }
[[ -d "$DIRTY" ]] || { print -u2 "dirty merged fixture was removed"; exit 1; }
[[ -d "$OWNED" ]] || { print -u2 "owned merged fixture was removed"; exit 1; }
kill -0 "$owned_pid" || { print -u2 "owned sleep died during retirement"; exit 1; }
[[ -f "$cursor" ]] || { print -u2 "missing shared cursor after wt-1"; exit 1; }
[[ ! -e "$WORKDIR/wt-1/.herd/worktree-reap-pulse.cursor" ]] || { print -u2 "wt-1 grew its own cursor"; exit 1; }
last1="$(cat "$cursor")"

out2="$(cd "$WORKDIR/wt-2" && "$HERD" maintenance --act 2>&1)"
log "$out2"
print -r -- "$out2" | grep -E -q 'eligible=(9|[1-9][0-9]+)' || {
  print -u2 "wt-2 eligible not >8: $out2"
  exit 1
}
print -r -- "$out2" | grep -E -q 'inspected=8([^0-9]|$)' || {
  print -u2 "wt-2 inspected not 8: $out2"
  exit 1
}
print -r -- "$out2" | grep -E -q 'retired=0' || {
  print -u2 "second beat must not retire remaining unmerged/dirty/owned: $out2"
  exit 1
}
[[ -d "$DIRTY" ]] || { print -u2 "dirty fixture missing after second beat"; exit 1; }
[[ -d "$OWNED" ]] || { print -u2 "owned fixture missing after second beat"; exit 1; }
[[ -f "$cursor" ]] || { print -u2 "shared cursor vanished"; exit 1; }
[[ ! -e "$WORKDIR/wt-2/.herd/worktree-reap-pulse.cursor" ]] || { print -u2 "wt-2 grew its own cursor"; exit 1; }
last2="$(cat "$cursor")"
[[ "$last1" != "$last2" ]] || { print -u2 "cursor last did not advance: $last1"; exit 1; }

integer w
for w in {1..12}; do
  [[ -d "$WORKDIR/wt-$w" ]] || { print -u2 "fixture wt-$w removed"; exit 1; }
done

dangle="$WORKDIR/dangle"
ln -s "$WORKDIR/nowhere-target" "$dangle"
set +e
dangle_out="$(cd "$WORKDIR/wt-1" && HERD_PROJECT_ROOT="$dangle" "$HERD" maintenance --act 2>&1)"
dangle_rc=$?
set -e
log "$dangle_out"
if (( dangle_rc == 0 )); then
  print -u2 "dangling HERD_PROJECT_ROOT must refuse: $dangle_out"
  exit 1
fi
print -r -- "$dangle_out" | grep -E -q 'dangling symlink|unresolvable identity' || {
  print -u2 "dangling refusal missing expected text: $dangle_out"
  exit 1
}

log "maintenance shared tick lab ok last1=$last1 last2=$last2"
