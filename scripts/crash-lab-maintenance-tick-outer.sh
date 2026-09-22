#!/usr/bin/env zsh
# Privileged hosted wrapper: provision one lab UID, run the whole inner script as that UID.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux outer wrapper only"
  exit 1
fi

HERD="${HERD:?HERD binary required}"
INNER="${INNER:-$(cd "$(dirname "$0")" && pwd)/crash-lab-maintenance-tick.sh}"
STAGE="$(mktemp -d /tmp/herd-maint-stage.XXXXXX)"
chmod 0755 "$STAGE"
labuser="herd-mlab-$$"
created=0
inner_rc=1
cleanup_ok=1

finish() {
  local rc=$inner_rc
  if [[ -f "$STAGE/maint-lab.log" ]]; then
    if [[ -n "${MAINT_LAB_LOG:-}" ]]; then
      cp "$STAGE/maint-lab.log" "$MAINT_LAB_LOG" 2>/dev/null || sudo -n cp "$STAGE/maint-lab.log" "$MAINT_LAB_LOG" || {
        print -u2 "failed to retrieve inner log"
        cleanup_ok=0
      }
    fi
  else
    print -u2 "missing inner log after run rc=$rc"
    cleanup_ok=0
  fi
  if [[ -n "${labuser:-}" && $created -eq 1 ]]; then
    leftover="$(pgrep -u "$labuser" || true)"
    if [[ -n "$leftover" ]]; then
      sudo -n pkill -u "$labuser" || true
      sleep 0.5
      leftover="$(pgrep -u "$labuser" || true)"
      if [[ -n "$leftover" ]]; then
        print -u2 "lab user still has processes: $leftover"
        cleanup_ok=0
      fi
    fi
    if sudo -n userdel "$labuser"; then
      if id "$labuser" >/dev/null 2>&1; then
        print -u2 "user $labuser still present after userdel"
        cleanup_ok=0
      fi
    else
      print -u2 "userdel $labuser failed"
      cleanup_ok=0
    fi
  fi
  if ! sudo -n rm -rf "$STAGE"; then
    print -u2 "staging rm failed"
    cleanup_ok=0
  fi
  if [[ -e "$STAGE" ]]; then
    print -u2 "staging path still exists"
    cleanup_ok=0
  fi
  if (( cleanup_ok == 0 )); then
    exit 1
  fi
  exit $rc
}
trap finish EXIT

sudo -n useradd --system --no-create-home --home-dir "$STAGE" --shell "$(command -v zsh)" "$labuser"
created=1
cp "$HERD" "$STAGE/herd"
cp "$INNER" "$STAGE/inner.sh"
chmod 0755 "$STAGE/herd" "$STAGE/inner.sh"
: >"$STAGE/maint-lab.log"
chmod 0644 "$STAGE/maint-lab.log"
sudo -n chown -R "$labuser:$labuser" "$STAGE"
sudo -n chmod 0755 "$STAGE"
sudo -n chmod 0644 "$STAGE/maint-lab.log"

set +e
sudo -n -H -u "$labuser" -- env -u XDG_CONFIG_HOME -u XDG_CONFIG_DIRS -u XDG_DATA_HOME -u XDG_CACHE_HOME \
  -u GIT_CONFIG_GLOBAL -u GIT_CONFIG_SYSTEM -u GIT_CONFIG_NOSYSTEM \
  -u HERD_ROOT -u HERD_REPO_ROOT -u HERD_PROJECT_ROOT -u HERD_CONFIG_PATH -u HERD_WORKSPACE \
  HERD="$STAGE/herd" MAINT_LAB_LOG="$STAGE/maint-lab.log" GITHUB_ACTIONS=true RUNNER_OS=Linux LANG=C \
  zsh "$STAGE/inner.sh"
inner_rc=$?
set -e
