#!/usr/bin/env zsh
# Ephemeral GitHub-hosted Linux runner only. Never run on the Mac.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" ]]; then
  print -u2 "refusing: GITHUB_ACTIONS is not true"
  exit 1
fi
if [[ "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: RUNNER_OS=${RUNNER_OS:-unset} is not Linux"
  exit 1
fi
if [[ "$(uname -s)" != "Linux" ]]; then
  print -u2 "refusing: kernel is not Linux"
  exit 1
fi
if [[ -z "${RUNNER_TEMP:-}" || ! -d "$RUNNER_TEMP" ]]; then
  print -u2 "refusing: RUNNER_TEMP missing"
  exit 1
fi

HERD="${HERD:?HERD binary path required}"
REPO="${REPO:?REPO path required}"
EVIDENCE="${EVIDENCE:?EVIDENCE path required}"
WORKDIR="$(mktemp -d "${RUNNER_TEMP}/herd-signer-lab.XXXXXX")"
KEYDIR="$WORKDIR/keys"
SOCK="$WORKDIR/signer.sock"
HERD_SIGNER_PID=""
created_group=""
created_users=()
orig_exit=0

log() { print -r -- "$*" | tee -a "$EVIDENCE"; }

fail() {
  orig_exit=$1
  shift
  print -u2 -r -- "$*"
  print -r -- "FAIL: $*" >>"$EVIDENCE"
  exit "$orig_exit"
}

cleanup() {
  local rc=$?
  set +e
  print -r -- "cleanup begin orig=$rc" >>"$EVIDENCE"
  if [[ -n "$HERD_SIGNER_PID" ]]; then
    if ! sudo -n kill -TERM "$HERD_SIGNER_PID" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: sudo kill -TERM pid=$HERD_SIGNER_PID failed" >>"$EVIDENCE"
    fi
    local waited=0
    while (( waited < 30 )) && sudo -n kill -0 "$HERD_SIGNER_PID" 2>/dev/null; do
      sleep 0.1
      waited=$((waited + 1))
    done
    if sudo -n kill -0 "$HERD_SIGNER_PID" 2>/dev/null; then
      if ! sudo -n kill -KILL "$HERD_SIGNER_PID" 2>>"$EVIDENCE"; then
        print -r -- "cleanup: sudo kill -KILL pid=$HERD_SIGNER_PID failed (process still present)" >>"$EVIDENCE"
        (( rc == 0 )) && rc=1
      fi
    fi
  fi
  local u
  for u in "${created_users[@]}"; do
    if ! sudo -n userdel "$u" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: userdel $u failed" >>"$EVIDENCE"
    fi
  done
  if [[ -n "$created_group" ]]; then
    if ! sudo -n groupdel "$created_group" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: groupdel $created_group failed" >>"$EVIDENCE"
    fi
  fi
  if [[ -d "$WORKDIR" ]]; then
    if ! sudo -n rm -rf "$WORKDIR" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: sudo rm workdir failed" >>"$EVIDENCE"
      (( rc == 0 )) && rc=1
    fi
  fi
  print -r -- "cleanup end rc=$rc" >>"$EVIDENCE"
  exit $rc
}
trap cleanup EXIT

: >"$EVIDENCE"
if [[ ! -x "$HERD" ]]; then
  fail 1 "HERD is not executable"
fi

sudo -n true || fail 1 "passwordless sudo required on ephemeral Linux runner"

group="herd-lab-sock-$$"
if ! sudo -n groupadd "$group"; then
  fail 1 "groupadd $group failed"
fi
created_group=$group

user_s="herd-lab-s-$$"
user_r="herd-lab-r-$$"
user_b="herd-lab-b-$$"
if ! sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin -g "$group" "$user_s"; then
  fail 1 "useradd $user_s failed"
fi
created_users+=("$user_s")
if ! sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin -g "$group" "$user_r"; then
  fail 1 "useradd $user_r failed"
fi
created_users+=("$user_r")
if ! sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin -g "$group" "$user_b"; then
  fail 1 "useradd $user_b failed"
fi
created_users+=("$user_b")

export HERD_SIGNER_UID="$(id -u "$user_s")"
export HERD_REQUESTER_UID="$(id -u "$user_r")"
export HERD_BUILDER_UID="$(id -u "$user_b")"
export HERD_SIGNER_SOCK_GID="$(getent group "$group" | cut -d: -f3)"
export HERD_KEY_DIR="$KEYDIR"
export HERD_SIGNER_SOCK="$SOCK"
log "topology S=$HERD_SIGNER_UID R=$HERD_REQUESTER_UID B=$HERD_BUILDER_UID G=$HERD_SIGNER_SOCK_GID"

if [[ "$HERD_SIGNER_UID" == "$HERD_REQUESTER_UID" || "$HERD_SIGNER_UID" == "$HERD_BUILDER_UID" || "$HERD_REQUESTER_UID" == "$HERD_BUILDER_UID" ]]; then
  fail 1 "UIDs are not distinct"
fi

sudo -n mkdir -p "$KEYDIR"
sudo -n chown "$HERD_REQUESTER_UID:$HERD_SIGNER_SOCK_GID" "$WORKDIR"
sudo -n chmod 0770 "$WORKDIR"

topo_env=(
  HERD_SIGNER_UID="$HERD_SIGNER_UID"
  HERD_REQUESTER_UID="$HERD_REQUESTER_UID"
  HERD_BUILDER_UID="$HERD_BUILDER_UID"
  HERD_SIGNER_SOCK_GID="$HERD_SIGNER_SOCK_GID"
  HERD_KEY_DIR="$KEYDIR"
  HERD_SIGNER_SOCK="$SOCK"
)

launch_out="$(sudo -n env "${topo_env[@]}" timeout 45s "$HERD" signer-boundary launch --key-dir "$KEYDIR" --socket "$SOCK" --repo "$REPO" --identity crash-lab 2> >(tee -a "$EVIDENCE" >&2))" || fail $? "launch failed"
print -r -- "$launch_out" | grep -E '^(HERD_SIGNER_PID|HERD_SIGNER_SOCK|HERD_ADMISSION_LEDGER|HERD_KEY_DIR|HERD_SEALED_SESSION)=' | tee -a "$EVIDENCE"
HERD_SIGNER_PID="$(print -r -- "$launch_out" | awk -F= '/^HERD_SIGNER_PID=/{print $2; exit}')"
[[ -n "$HERD_SIGNER_PID" ]] || fail 1 "launch did not print HERD_SIGNER_PID"

# status/prove as requester using ResolveKeyDir (HERD_KEY_DIR), not guessed extra flags.
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary status >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "status failed as requester"
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 30s "$HERD" signer-boundary prove --repo "$REPO" --identity crash-lab >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "prove failed as requester"

# revoke requires --key-dir per CLI contract.
sudo -n env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary revoke --key-dir "$KEYDIR" --identity crash-lab --socket "$SOCK" >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "revoke failed"

log "ok"
