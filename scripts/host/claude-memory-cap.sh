#!/usr/bin/env bash
# Memory-bounded Claude launch that PRESERVES PROCESS IDENTITY.
# FAC-692 -- 2026-08-26 W4 incident, third iteration.
#
# WHY NOT systemd-run
# -------------------
# The previous version did `exec systemd-run --user --scope ... claude`. That
# works as a cap and BREAKS THE FLEET: systemd-run forks the command, so the
# pane's foreground process is systemd-run and Claude is its child. herdr
# identifies an agent by inspecting the pane process's cmdline, so it never saw
# the agent:
#
#   herdr agent start -> {"error":{"code":"timeout","message":"timed out waiting for agent startup"}}
#   herdr agent get   -> agent_not_found
#   pane content      -> Claude running and fully interactive
#
# Claude was fine. herdr could not see it. A launch that fails is strictly worse
# than a launch that is uncapped, so the cap must not change what the pane's
# process IS.
#
# WHAT THIS DOES INSTEAD
# ----------------------
# systemd-run --user --scope for the cgroup, with argv[0] forced back to
# "claude" INSIDE the scope.
#
# A self-move into a delegated app.slice cgroup was tried first and cannot work
# here: cgroup v2 refuses to move a process across a delegation boundary, and
# herdr must live in the user's terminal session scope (it drives Ghostty and
# dies with "ghostty error -2" if relocated into a headless systemd unit). So
# the panes are permanently outside user@1000.service and the move is refused.
#
# The argv[0] correction is the part that was missing all along. herdr
# identifies an agent from the pane process's argv[0]; `exec systemd-run ...
# "$REAL"` presents the versioned install path there, so herdr never recognised
# the agent and reported a startup timeout while Claude ran fine.
#
# FAILS OPEN, LOUDLY. Every path ends in `exec "$REAL"`. If the cgroup cannot be
# created or written, Claude still launches uncapped and the reason is appended
# to a log. An uncapped review is bounded by the memory, swap and derived-slot
# gates; a harness that will not start is a certain outage on every launch.
#
# Reverting: point ~/.local/bin/claude back at the path in ~/.claude-real-target.
set -uo pipefail

REAL="$(cat "$HOME/.claude-real-target" 2>/dev/null || true)"
if [ -z "$REAL" ] || [ ! -x "$REAL" ]; then
  echo "claude wrapper: real claude binary not found (expected path in ~/.claude-real-target)" >&2
  exit 127
fi

MEM="${HERD_AGENT_MEMORY_MAX:-4G}"
LOG="$HOME/.herd/logs/uncapped-launches.log"

note_uncapped() {
  mkdir -p "$(dirname "$LOG")" 2>/dev/null
  printf '%s pid=%s cwd=%s reason=%s\n' \
    "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$$" "$PWD" "$1" >> "$LOG" 2>/dev/null
}

# A pane spawned by a detached herdr can lack the user-session environment, so
# `systemd-run --user` would fail for want of a bus rather than of systemd.
: "${XDG_RUNTIME_DIR:=/run/user/$(id -u)}"
export XDG_RUNTIME_DIR
if [ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ] && [ -S "$XDG_RUNTIME_DIR/bus" ]; then
  export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
fi

# running/degraded/maintenance/starting all mean the user manager is alive.
# This host reports "degraded" from an unrelated failed unit, and scopes
# demonstrably work in that state; treating degraded as "no systemd" was an
# earlier bug that made this fall open while reporting itself capped.
state=$(systemctl --user is-system-running 2>/dev/null)
case "$state" in
  running|degraded|maintenance|starting|initializing)
    if command -v systemd-run >/dev/null 2>&1; then
      # `exec -a claude` runs INSIDE the scope so the final process carries both
      # the cgroup limit and the argv herdr matches on.
      exec systemd-run --user --scope --quiet \
        -p MemoryMax="$MEM" -p MemorySwapMax=1G -p TasksMax=512 \
        -- bash -c 'exec -a claude "$0" "$@"' "$REAL" "$@"
    fi
    note_uncapped "systemd-run not available"
    ;;
  *)
    note_uncapped "user systemd manager state=${state:-unreachable}"
    ;;
esac

# Fail open: a harness that will not start is a certain outage on every launch,
# while an uncapped review is bounded by the memory, swap and slot gates. The
# reason is always logged, so it can never be silent.
note_uncapped "launching without a memory cap"
exec -a claude "$REAL" "$@"
