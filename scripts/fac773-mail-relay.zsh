#!/usr/bin/env zsh

emulate -L zsh
setopt errexit nounset pipefail

usage() {
  print -u2 "usage: $0 --host wsl-box --remote-binary PATH --remote-mail PATH --local-binary PATH --local-mail PATH --recipient NAME --workspace ID [--connect-timeout SEC] [--command-timeout SEC] [--watch-timeout SEC]"
}

host=""
remote_binary=""
remote_mail=""
local_binary=""
local_mail=""
recipient=""
workspace=""
connect_timeout=5
command_timeout=30
watch_timeout=5

while (( $# )); do
  case "$1" in
    --host) host="${2:-}"; shift 2 ;;
    --remote-binary) remote_binary="${2:-}"; shift 2 ;;
    --remote-mail) remote_mail="${2:-}"; shift 2 ;;
    --local-binary) local_binary="${2:-}"; shift 2 ;;
    --local-mail) local_mail="${2:-}"; shift 2 ;;
    --recipient) recipient="${2:-}"; shift 2 ;;
    --workspace) workspace="${2:-}"; shift 2 ;;
    --connect-timeout) connect_timeout="${2:-}"; shift 2 ;;
    --command-timeout) command_timeout="${2:-}"; shift 2 ;;
    --watch-timeout) watch_timeout="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) print -u2 "relay: unknown or incomplete option: $1"; usage; exit 2 ;;
  esac
done

[[ "$host" == "wsl-box" ]] || { print -u2 "relay: --host must be the explicit supported host wsl-box"; exit 2; }
for value in "$remote_binary" "$remote_mail" "$local_binary" "$local_mail" "$recipient" "$workspace"; do
  case "$value" in
    ""|*[!A-Za-z0-9_./:-]*) print -u2 "relay: empty or unsafe argument"; exit 2 ;;
  esac
done
[[ "$remote_mail" == /* && "$local_mail" == /* ]] || { print -u2 "relay: remote and local mailbox paths must be absolute canonical paths"; exit 2; }
[[ "$connect_timeout" == <-> && "$command_timeout" == <-> && "$watch_timeout" == <-> ]] || { print -u2 "relay: timeout values must be non-negative integers"; exit 2; }
command -v ssh >/dev/null || { print -u2 "relay: ssh is required"; exit 1; }
command -v jq >/dev/null || { print -u2 "relay: jq is required for native JSON status parsing"; exit 1; }

tmpdir="$(mktemp -d "${TMPDIR:-/tmp}/fac773-mail-relay.XXXXXX")" || exit 1
trap 'rm -rf -- "$tmpdir"' EXIT INT TERM
remote_json="$tmpdir/remote.json"
imported_json="$tmpdir/imported.json"
watch_out="$tmpdir/watch.out"
watch_err="$tmpdir/watch.err"

run_bounded() {
  local seconds="$1"
  shift
  "$@" &
  local child=$!
  ( sleep "$seconds"; kill "$child" 2>/dev/null || true ) &
  local timer=$!
  local rc=0
  wait "$child" || rc=$?
  kill "$timer" 2>/dev/null || true
  wait "$timer" 2>/dev/null || true
  return "$rc"
}

ssh_opts=(-o BatchMode=yes -o "ConnectTimeout=$connect_timeout" -o ServerAliveInterval=2 -o ServerAliveCountMax=1)
if ! run_bounded "$command_timeout" ssh "${ssh_opts[@]}" "$host" "$remote_binary" mail pending --recipient "$recipient" --mail "$remote_mail" >"$remote_json"; then
  print -u2 "relay: remote pending collection failed; remote state remains pending"
  exit 1
fi
if ! jq -e 'type == "array"' "$remote_json" >/dev/null; then
  print -u2 "relay: remote pending output is not a JSON array"
  exit 1
fi
if jq -e 'length == 0' "$remote_json" >/dev/null; then
  exit 0
fi

if ! run_bounded "$command_timeout" "$local_binary" mail import --source-host "$host" --recipient "$recipient" --file "$remote_json" --mail "$local_mail" >"$imported_json"; then
  print -u2 "relay: local import failed; remote state remains pending"
  exit 1
fi
if ! jq -e --arg host "$host" --arg recipient "$recipient" 'type == "array" and all(.[]; .original_source_host == $host and .recipient == $recipient and (.original_source_id | type == "string"))' "$imported_json" >/dev/null; then
  print -u2 "relay: local import identity validation failed"
  exit 1
fi

# The native safe-boundary consumer performs at most one prompt per idle/done
# observation and writes the existing local handled sidecar only after proof.
watch_rc=0
run_bounded "$command_timeout" "$local_binary" watch --wake --recipient "$recipient" --workspace "$workspace" --mail "$local_mail" --interval 1 --timeout "$watch_timeout" >"$watch_out" 2>"$watch_err" || watch_rc=$?
if (( watch_rc != 0 && watch_rc != 2 )); then
  cat "$watch_err" >&2
  print -u2 "relay: local wake failed; remote state remains pending"
  exit 1
fi

pending=0
while IFS=$'\t' read -r local_id source_id; do
  [[ -n "$local_id" && -n "$source_id" ]] || continue
  status_file="$tmpdir/status-$local_id.json"
  if ! run_bounded "$command_timeout" "$local_binary" mail status --recipient "$recipient" --id "$local_id" --mail "$local_mail" >"$status_file"; then
    print -u2 "relay: local status failed for $local_id; remote state remains pending"
    exit 1
  fi
  if ! jq -e '.handled == true and .pending == false' "$status_file" >/dev/null; then
    pending=1
    continue
  fi
  if ! run_bounded "$command_timeout" ssh "${ssh_opts[@]}" "$host" "$remote_binary" mail ack --recipient "$recipient" --id "$source_id" --mail "$remote_mail" >/dev/null; then
    print -u2 "relay: remote ack failed for $source_id; retry is safe without local resubmission"
    exit 1
  fi
done < <(jq -r '.[] | [.id, .original_source_id] | @tsv' "$imported_json")

if (( pending )); then
  print -u2 "relay: recipient was busy or local consumption was not proven; remote state remains pending"
  exit 2
fi
