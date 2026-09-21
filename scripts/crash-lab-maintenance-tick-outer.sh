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
labuser="herd-mlab-$$"
created=0
inner_rc=1
cleanup_ok=1

finish() {
  local rc=$inner_rc
  if [[ -n "${labuser:-}" && $created -eq 1 ]]; then
    sudo -n pkill -u "$labuser" 2>/dev/null || true
    sleep 0.2
    if sudo -n userdel "$labuser"; then
      :
    else
      print -u2 "userdel $labuser failed"
      cleanup_ok=0
    fi
  fi
  if ! rm -rf "$STAGE"; then
    print -u2 "staging rm failed"
    cleanup_ok=0
  fi
  if (( cleanup_ok == 0 )); then
    exit 1
  fi
  exit $rc
}
trap finish EXIT

sudo -n useradd --system --no-create-home --home-dir "$STAGE" --shell /bin/zsh "$labuser"
created=1
cp "$HERD" "$STAGE/herd"
cp "$INNER" "$STAGE/inner.sh"
chmod 0755 "$STAGE/herd" "$STAGE/inner.sh"
: >"$STAGE/maint-lab.log"
sudo -n chown -R "$labuser:$labuser" "$STAGE"

set +e
sudo -n -u "$labuser" -- env -u HERD_ROOT -u HERD_REPO_ROOT -u HERD_PROJECT_ROOT -u HERD_CONFIG_PATH -u HERD_WORKSPACE \
  HERD="$STAGE/herd" MAINT_LAB_LOG="$STAGE/maint-lab.log" GITHUB_ACTIONS=true RUNNER_OS=Linux \
  zsh "$STAGE/inner.sh"
inner_rc=$?
set -e

if [[ -n "${MAINT_LAB_LOG:-}" && -f "$STAGE/maint-lab.log" ]]; then
  cp "$STAGE/maint-lab.log" "$MAINT_LAB_LOG" || cleanup_ok=0
fi
