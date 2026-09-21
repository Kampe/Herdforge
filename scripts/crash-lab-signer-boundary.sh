#!/usr/bin/env bash
# Ephemeral Ubuntu runner only. Never run on the Mac.
set -euo pipefail

HERD="${HERD:-}"
REPO="${REPO:-.}"
WORKDIR="${WORKDIR:-${RUNNER_TEMP:-/tmp}/herd-signer-lab}"
KEYDIR="$WORKDIR/keys"
SOCK="$WORKDIR/signer.sock"
EVIDENCE="${EVIDENCE:-$WORKDIR/evidence.txt}"
GROUP="herd-lab-sock"
USER_S="herd-lab-s"
USER_R="herd-lab-r"
USER_B="herd-lab-b"
HERD_SIGNER_PID=""

log() { printf '%s\n' "$*" | tee -a "$EVIDENCE" >/dev/null; printf '%s\n' "$*"; }

cleanup() {
  set +e
  if [[ -n "${HERD_SIGNER_PID:-}" ]]; then
    kill -TERM "$HERD_SIGNER_PID" 2>/dev/null || true
    timeout 3s bash -c "while kill -0 $HERD_SIGNER_PID 2>/dev/null; do sleep 0.1; done" || kill -KILL "$HERD_SIGNER_PID" 2>/dev/null || true
  fi
  if [[ -n "${HERD_SIGNER_UID:-}" ]]; then
    pgrep -u "$HERD_SIGNER_UID" -a 2>/dev/null | while read -r line; do
      case "$line" in
        *signer-boundary*) kill -TERM "${line%% *}" 2>/dev/null || true ;;
      esac
    done
  fi
  sudo -n userdel "$USER_S" 2>/dev/null || true
  sudo -n userdel "$USER_R" 2>/dev/null || true
  sudo -n userdel "$USER_B" 2>/dev/null || true
  sudo -n groupdel "$GROUP" 2>/dev/null || true
  rm -rf "$WORKDIR"
}
trap cleanup EXIT

if [[ -z "$HERD" || ! -x "$HERD" ]]; then
  echo "HERD binary missing or not executable: ${HERD:-unset}" >&2
  exit 1
fi
mkdir -p "$WORKDIR" "$KEYDIR"
: >"$EVIDENCE"

sudo -n groupadd -f "$GROUP"
sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin -g "$GROUP" "$USER_S"
sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin -g "$GROUP" "$USER_R"
sudo -n useradd --system --no-create-home --shell /usr/sbin/nologin -g "$GROUP" "$USER_B"

export HERD_SIGNER_UID
export HERD_REQUESTER_UID
export HERD_BUILDER_UID
export HERD_SIGNER_SOCK_GID
HERD_SIGNER_UID="$(id -u "$USER_S")"
HERD_REQUESTER_UID="$(id -u "$USER_R")"
HERD_BUILDER_UID="$(id -u "$USER_B")"
HERD_SIGNER_SOCK_GID="$(getent group "$GROUP" | cut -d: -f3)"
log "topology S=$HERD_SIGNER_UID R=$HERD_REQUESTER_UID B=$HERD_BUILDER_UID G=$HERD_SIGNER_SOCK_GID"

if [[ "$HERD_SIGNER_UID" == "$HERD_REQUESTER_UID" || "$HERD_SIGNER_UID" == "$HERD_BUILDER_UID" || "$HERD_REQUESTER_UID" == "$HERD_BUILDER_UID" ]]; then
  echo "UIDs are not distinct" >&2
  exit 1
fi

launch_out="$(timeout 45s env \
  HERD_SIGNER_UID="$HERD_SIGNER_UID" \
  HERD_REQUESTER_UID="$HERD_REQUESTER_UID" \
  HERD_BUILDER_UID="$HERD_BUILDER_UID" \
  HERD_SIGNER_SOCK_GID="$HERD_SIGNER_SOCK_GID" \
  "$HERD" signer-boundary launch --key-dir "$KEYDIR" --socket "$SOCK" --repo "$REPO" --identity crash-lab)"
printf '%s\n' "$launch_out" | tee -a "$EVIDENCE"
HERD_SIGNER_PID="$(printf '%s\n' "$launch_out" | awk -F= '/^HERD_SIGNER_PID=/{print $2; exit}')"
if [[ -z "$HERD_SIGNER_PID" ]]; then
  echo "launch did not print HERD_SIGNER_PID" >&2
  exit 1
fi
log "launch_pid=$HERD_SIGNER_PID"

timeout 20s env \
  HERD_SIGNER_UID="$HERD_SIGNER_UID" \
  HERD_REQUESTER_UID="$HERD_REQUESTER_UID" \
  HERD_BUILDER_UID="$HERD_BUILDER_UID" \
  HERD_SIGNER_SOCK_GID="$HERD_SIGNER_SOCK_GID" \
  "$HERD" signer-boundary status --key-dir "$KEYDIR" | tee -a "$EVIDENCE"

timeout 30s env \
  HERD_SIGNER_UID="$HERD_SIGNER_UID" \
  HERD_REQUESTER_UID="$HERD_REQUESTER_UID" \
  HERD_BUILDER_UID="$HERD_BUILDER_UID" \
  HERD_SIGNER_SOCK_GID="$HERD_SIGNER_SOCK_GID" \
  "$HERD" signer-boundary prove --key-dir "$KEYDIR" --repo "$REPO" --identity crash-lab | tee -a "$EVIDENCE"

timeout 20s env \
  HERD_SIGNER_UID="$HERD_SIGNER_UID" \
  HERD_REQUESTER_UID="$HERD_REQUESTER_UID" \
  HERD_BUILDER_UID="$HERD_BUILDER_UID" \
  HERD_SIGNER_SOCK_GID="$HERD_SIGNER_SOCK_GID" \
  HERD_SIGNER_SOCK="$SOCK" \
  "$HERD" signer-boundary revoke --key-dir "$KEYDIR" --identity crash-lab --socket "$SOCK" | tee -a "$EVIDENCE"

# Never copy key/sealed session into evidence or dist.
if grep -R --include='*' -l . "$KEYDIR" >/dev/null 2>&1; then
  log "key_dir_present_not_uploaded=1"
fi
log "ok"
