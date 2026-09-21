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
HERD="$(readlink -f "$HERD")"
REPO="${REPO:?REPO path required}"
EVIDENCE="${EVIDENCE:?EVIDENCE path required}"
WORKDIR="$(mktemp -d /tmp/herd-signer-lab.XXXXXX)"
KEYDIR="$WORKDIR/keys"
SOCKDIR="$WORKDIR/sock"
SOCK="$SOCKDIR/signer.sock"
HERD_SIGNER_PID=""
created_group=""
created_users=()

log() { print -r -- "$*" | tee -a "$EVIDENCE"; }

diag_path() {
  local p=$1
  if [[ -e "$p" ]]; then
    sudo -n stat -c 'mode=%a owner=%U:%G path=%n' "$p" 2>/dev/null | tee -a "$EVIDENCE" || true
  else
    log "diag missing $p"
  fi
}

diag_ancestry() {
  local p=$1
  log "ancestry $p"
  while true; do
    diag_path "$p"
    [[ "$p" == / ]] && break
    p="$(dirname "$p")"
  done
}

diag_dirs() {
  local p
  for p in /tmp "$WORKDIR" "$KEYDIR" "$KEYDIR/private" "$KEYDIR/attest" "$SOCKDIR"; do
    diag_path "$p"
  done
  if [[ -n "${RUNNER_TEMP:-}" ]]; then
    log "hypothesis: RUNNER_TEMP ancestry (unchanged)"
    diag_ancestry "$RUNNER_TEMP"
  fi
}

fail() {
  local code=$1
  shift
  print -u2 -r -- "$*"
  print -r -- "FAIL: $*" >>"$EVIDENCE"
  exit "$code"
}

numeric_pid() { [[ -n "${1:-}" && "$1" == <-> && "$1" -gt 1 ]]; }

lab_owned_pid() {
  local pid=$1
  numeric_pid "$pid" || return 1
  [[ -n "${HERD_SIGNER_UID:-}" ]] || return 1
  local uid exe cmd
  uid="$(ps -o uid= -p "$pid" 2>/dev/null | awk '{print $1}')"
  [[ "$uid" == "$HERD_SIGNER_UID" ]] || return 1
  exe="$(sudo -n readlink "/proc/$pid/exe" 2>/dev/null || true)"
  cmd="$(ps -o args= -p "$pid" 2>/dev/null || true)"
  [[ "$exe" == "$HERD" || "$exe" == "$HERD (deleted)" || "$cmd" == "$HERD "* || "$cmd" == *"/herd-linux-amd64 "* ]] || return 1
  [[ "$cmd" == *signer-boundary* || "$exe" == "$HERD" || "$exe" == "$HERD (deleted)" ]] || return 1
}

stop_lab_pid() {
  local pid=$1
  if ! lab_owned_pid "$pid"; then
    print -r -- "cleanup: skip pid=$pid (not numeric lab-owned herd signer)" >>"$EVIDENCE"
    return 0
  fi
  if ! sudo -n kill -TERM "$pid" 2>>"$EVIDENCE"; then
    print -r -- "cleanup: TERM pid=$pid failed" >>"$EVIDENCE"
    return 1
  fi
  local waited=0
  while (( waited < 30 )) && sudo -n kill -0 "$pid" 2>/dev/null; do
    sleep 0.1
    waited=$((waited + 1))
  done
  if sudo -n kill -0 "$pid" 2>/dev/null; then
    if ! sudo -n kill -KILL "$pid" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: KILL pid=$pid failed" >>"$EVIDENCE"
      return 1
    fi
    waited=0
    while (( waited < 20 )) && sudo -n kill -0 "$pid" 2>/dev/null; do
      sleep 0.1
      waited=$((waited + 1))
    done
  fi
  if sudo -n kill -0 "$pid" 2>/dev/null; then
    print -r -- "cleanup: pid=$pid still present after KILL" >>"$EVIDENCE"
    return 1
  fi
  return 0
}

collect_signer_pids() {
  local pids=() p
  if numeric_pid "${HERD_SIGNER_PID:-}"; then
    pids+=("$HERD_SIGNER_PID")
  fi
  if [[ -n "${HERD_SIGNER_UID:-}" ]]; then
    for p in $(ps -o pid= -u "$HERD_SIGNER_UID" 2>/dev/null); do
      pids+=("$p")
    done
  fi
  print -l -- "${pids[@]}" | awk 'NF && !seen[$0]++'
}

cleanup() {
  local rc=$?
  local dirty=0
  set +e
  print -r -- "cleanup begin orig=$rc" >>"$EVIDENCE"
  local p
  for p in $(collect_signer_pids); do
    if ! stop_lab_pid "$p"; then
      dirty=1
    fi
  done
  local u
  for u in "${created_users[@]}"; do
    if ! sudo -n userdel "$u" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: userdel $u failed" >>"$EVIDENCE"
      dirty=1
    fi
  done
  if [[ -n "$created_group" ]]; then
    if ! sudo -n groupdel "$created_group" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: groupdel $created_group failed" >>"$EVIDENCE"
      dirty=1
    fi
  fi
  if [[ -d "$WORKDIR" ]]; then
    if ! sudo -n rm -rf "$WORKDIR" 2>>"$EVIDENCE"; then
      print -r -- "cleanup: sudo rm workdir failed" >>"$EVIDENCE"
      dirty=1
    fi
  fi
  if (( dirty )); then
    if (( rc == 0 )); then
      rc=1
    fi
  fi
  print -r -- "cleanup end rc=$rc dirty=$dirty" >>"$EVIDENCE"
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

sudo -n chmod 0755 "$WORKDIR"
sudo -n mkdir -p "$KEYDIR" "$SOCKDIR" "$WORKDIR/bin" "$WORKDIR/repo"
sudo -n chmod 0755 "$KEYDIR" "$WORKDIR/bin"
sudo -n cp "$HERD" "$WORKDIR/bin/herd-linux-amd64"
sudo -n chmod 0755 "$WORKDIR/bin/herd-linux-amd64"
HERD="$WORKDIR/bin/herd-linux-amd64"
sudo -n git -C "$WORKDIR/repo" init -q
sudo -n chown -R "$HERD_REQUESTER_UID:$HERD_SIGNER_SOCK_GID" "$WORKDIR/repo"
sudo -n chmod -R u+rwX,g+rwX "$WORKDIR/repo"
sudo -n chown "$HERD_SIGNER_UID:$HERD_SIGNER_SOCK_GID" "$SOCKDIR"
sudo -n chmod 0770 "$SOCKDIR"
REPO="$WORKDIR/repo"
log "setpriv=$(command -v setpriv || print none)"
sudo -n -u "#$HERD_SIGNER_UID" -- id | tee -a "$EVIDENCE" || true
sudo -n -u "#$HERD_REQUESTER_UID" -- id | tee -a "$EVIDENCE" || true
sudo -n -u "#$HERD_BUILDER_UID" -- id | tee -a "$EVIDENCE" || true
if command -v setpriv >/dev/null 2>&1; then
  sudo -n setpriv --reuid="$HERD_SIGNER_UID" --regid="$HERD_SIGNER_SOCK_GID" --init-groups -- id | tee -a "$EVIDENCE" || true
fi
diag_dirs

topo_env=(
  HERD_SIGNER_UID="$HERD_SIGNER_UID"
  HERD_REQUESTER_UID="$HERD_REQUESTER_UID"
  HERD_BUILDER_UID="$HERD_BUILDER_UID"
  HERD_SIGNER_SOCK_GID="$HERD_SIGNER_SOCK_GID"
  HERD_KEY_DIR="$KEYDIR"
  HERD_SIGNER_SOCK="$SOCK"
)

set +e
launch_out="$(sudo -n env "${topo_env[@]}" timeout 45s "$HERD" signer-boundary launch --key-dir "$KEYDIR" --socket "$SOCK" --repo "$REPO" --identity crash-lab 2> >(tee -a "$EVIDENCE" >&2))"
launch_rc=$?
set -e
if (( launch_rc != 0 )); then
  diag_dirs
  fail "$launch_rc" "launch failed rc=$launch_rc"
fi
print -r -- "$launch_out" | grep -E '^(HERD_SIGNER_PID|HERD_SIGNER_SOCK|HERD_ADMISSION_LEDGER|HERD_KEY_DIR|HERD_SEALED_SESSION)=' | tee -a "$EVIDENCE"
HERD_SIGNER_PID="$(print -r -- "$launch_out" | awk -F= '/^HERD_SIGNER_PID=/{print $2; exit}')"
[[ -n "$HERD_SIGNER_PID" ]] || fail 1 "launch did not print HERD_SIGNER_PID"

pos_out="$WORKDIR/audit-pos.out"
pos_err="$WORKDIR/audit-pos.err"
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary audit-key --repo "$REPO" --identity crash-lab >"$pos_out" 2>"$pos_err" || fail $? "positive audit-key failed $(cat "$pos_err")"
grep -q "audit-key ok" "$pos_out" || fail 1 "positive audit-key missing ok: $(cat "$pos_out" "$pos_err")"
cat "$pos_out" "$pos_err" >>"$EVIDENCE"
log "positive audit-key ok"

wrong_err="$WORKDIR/audit-wrong.err"
set +e
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary audit-key --repo "$REPO" --identity wrong-id >/dev/null 2>"$wrong_err"
wrong_rc=$?
set -e
cat "$wrong_err" >>"$EVIDENCE"
if (( wrong_rc == 0 )); then
  fail 1 "wrong identity audit-key should fail"
fi
grep -E -q "identity mismatch|audit identity" "$wrong_err" || fail 1 "wrong-id missing identity mismatch: $(cat "$wrong_err")"
log "negative wrong-identity rc=$wrong_rc"

replay_nonce="00112233445566778899aabbccddeeff"
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary audit-key --repo "$REPO" --identity crash-lab --nonce "$replay_nonce" >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "first replay nonce should succeed"
replay_err="$WORKDIR/audit-replay.err"
set +e
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary audit-key --repo "$REPO" --identity crash-lab --nonce "$replay_nonce" >/dev/null 2>"$replay_err"
replay_rc=$?
set -e
cat "$replay_err" >>"$EVIDENCE"
if (( replay_rc == 0 )); then
  fail 1 "replayed audit-key nonce should fail"
fi
grep -E -q "NONCE_REPLAY|nonce replay|replay" "$replay_err" || fail 1 "replay missing replay reason: $(cat "$replay_err")"
log "negative replay rc=$replay_rc"

# Authentic establish as requester (writes attest/isolation.json). Do not
# synthesize attestation or weaken RequireReady.
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 45s "$HERD" signer-boundary establish --repo "$REPO" --identity crash-lab >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "establish failed as requester"

# status/prove as requester using ResolveKeyDir (HERD_KEY_DIR).
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary status >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "status failed as requester"
sudo -n -u "#$HERD_REQUESTER_UID" env "${topo_env[@]}" timeout 30s "$HERD" signer-boundary prove --repo "$REPO" --identity crash-lab >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "prove failed as requester"

# revoke requires --key-dir per CLI contract.
sudo -n env "${topo_env[@]}" timeout 20s "$HERD" signer-boundary revoke --key-dir "$KEYDIR" --identity crash-lab --socket "$SOCK" >>"$EVIDENCE" 2> >(tee -a "$EVIDENCE" >&2) || fail $? "revoke failed"

log "ok"
