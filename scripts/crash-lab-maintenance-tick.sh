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
cleanup() { rm -rf "$WORKDIR"; }
trap cleanup EXIT

git -C "$WORKDIR" init -q
git -C "$WORKDIR" config user.email lab@example.com
git -C "$WORKDIR" config user.name lab
print -r -- 'ok' >"$WORKDIR/f"
git -C "$WORKDIR" add f
git -C "$WORKDIR" commit -qm init
git -C "$WORKDIR" branch -M main
git -C "$WORKDIR" checkout -qb leftover
print -r -- 'x' >"$WORKDIR/g"
git -C "$WORKDIR" add g
git -C "$WORKDIR" commit -qm leftover
git -C "$WORKDIR" checkout -q main

git -C "$WORKDIR" worktree add -q "$WORKDIR/wt-a" leftover
git -C "$WORKDIR" checkout -qb leftover-b
print -r -- 'y' >"$WORKDIR/h"
git -C "$WORKDIR" add h
git -C "$WORKDIR" commit -qm leftover-b
git -C "$WORKDIR" worktree add -q "$WORKDIR/wt-b" leftover-b
git -C "$WORKDIR" checkout -q main

mkdir -p "$WORKDIR/.herd"
print -r -- "project:\n  name: maint-lab\n  default_branch: main\n" >"$WORKDIR/.herd/herd.yaml"

# Shared lock exclusion: hold project lock, maintenance from a linked worktree must defer.
touch "$WORKDIR/.herd/worktree-reap-pulse.lock"
flock -n "$WORKDIR/.herd/worktree-reap-pulse.lock" sleep 20 &
holder=$!
sleep 0.2
set +e
out="$(cd "$WORKDIR/wt-a" && "$HERD" maintenance --act 2>&1)"
rc=$?
set -e
kill "$holder" 2>/dev/null || true
wait "$holder" 2>/dev/null || true
print -r -- "$out"
print -r -- "$out" | grep -E -q 'deferred|tick lock' || {
  print -u2 "missing deferred/tick lock while flock held (rc=$rc): $out"
  exit 1
}

# After release, both linked checkouts must write the SAME project cursor.
cd "$WORKDIR/wt-a"
"$HERD" maintenance --act
cursor_a="$WORKDIR/.herd/worktree-reap-pulse.cursor"
[[ -f "$cursor_a" ]] || { print -u2 "missing shared cursor after wt-a"; exit 1; }
[[ ! -e "$WORKDIR/wt-a/.herd/worktree-reap-pulse.cursor" ]] || { print -u2 "wt-a grew its own cursor"; exit 1; }
ino_a="$(ls -i "$cursor_a" | awk '{print $1}')"
last_a="$(cat "$cursor_a")"

cd "$WORKDIR/wt-b"
"$HERD" maintenance --act
[[ -f "$cursor_a" ]] || { print -u2 "shared cursor vanished"; exit 1; }
[[ ! -e "$WORKDIR/wt-b/.herd/worktree-reap-pulse.cursor" ]] || { print -u2 "wt-b grew its own cursor"; exit 1; }
ino_b="$(ls -i "$cursor_a" | awk '{print $1}')"
last_b="$(cat "$cursor_a")"
[[ "$ino_a" == "$ino_b" ]] || { print -u2 "cursor inode changed path identity"; exit 1; }
[[ "$last_b" != "$last_a" ]] || print -r -- "cursor last unchanged (eligible set may be singleton); inode shared"

print -r -- "maintenance shared tick lab ok"
