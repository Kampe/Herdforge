#!/usr/bin/env bash
# W4 (wsl-box) host hardening — the four items that need root.
# Written after the 2026-08-26 review-fleet memory-thrash incident.
#
# RUN WITH:  sudo bash ~/w4-harden.sh
# DRY RUN:   sudo bash ~/w4-harden.sh --dry-run
#
# Every step is idempotent and prints what it changed. Nothing here restarts
# herdr or touches a running review.
#
# WHAT HAPPENED, so the numbers below are not arbitrary. A Bun worker hit a
# kernel page-allocation failure with ~41GB anonymous memory against a 48GB VM
# ceiling, 5GB swap in active writeback, the Normal zone at its minimum
# watermark, and zero contiguous blocks >=512KB. The VM thrashed and the whole
# machine went unresponsive. Per-agent cgroup caps (4G) are already live and are
# the control that prevents recurrence; these four are defence in depth.
set -uo pipefail

DRY=0
[ "${1:-}" = "--dry-run" ] && DRY=1

if [ "$(id -u)" -ne 0 ]; then
  echo "must run as root: sudo bash $0" >&2
  exit 1
fi

TARGET_USER="${SUDO_USER:-kampe}"
TARGET_UID="$(id -u "$TARGET_USER")"

run() {
  if [ "$DRY" = "1" ]; then
    echo "  WOULD RUN: $*"
  else
    "$@"
  fi
}

write_file() {
  # write_file <path> <<'EOF' ... EOF   (content on stdin)
  local path="$1" content
  content="$(cat)"
  mkdir -p "$(dirname "$path")"
  if [ -f "$path" ] && [ "$(cat "$path")" = "$content" ]; then
    echo "  unchanged: $path"
    return
  fi
  if [ "$DRY" = "1" ]; then
    echo "  WOULD WRITE: $path"
    return
  fi
  printf '%s\n' "$content" > "$path"
  echo "  wrote: $path"
}

echo "==> 1/4  Early OOM defense: systemd-oomd + swappiness"
# systemd-oomd kills the worst cgroup on sustained memory PRESSURE, before the
# kernel is reduced to failing allocations. swappiness 60 let the VM burn ~5GB
# of swap first, which IS the thrash window that froze the box; 10 makes the
# kernel reclaim page cache long before it starts paging anonymous memory out.
write_file /etc/sysctl.d/99-herd-swappiness.conf <<'EOF'
# 2026-08-26 review-fleet thrash: default swappiness=60 let the VM consume ~5GB
# of swap with active writeback before anything was killed. Reclaim cache first.
vm.swappiness = 10
EOF
run sysctl --quiet -w vm.swappiness=10
if systemctl list-unit-files systemd-oomd.service >/dev/null 2>&1; then
  run systemctl enable --now systemd-oomd.service
  echo "  systemd-oomd: $(systemctl is-active systemd-oomd.service 2>/dev/null)"
else
  echo "  systemd-oomd: NOT PRESENT on this image (skipped, not failed)"
fi

echo "==> 2/4  Session-wide backstop: user@${TARGET_UID}.service MemoryMax"
# THE HIGHEST-VALUE REMAINING PROTECTION. Per-agent caps only bind launches that
# go through the wrapper, and that wrapper lives on a symlink Claude's own
# updater owns and may replace on upgrade. This bounds EVERYTHING the user runs,
# including an unwrapped launch, so the VM cannot be driven to its ceiling at
# all.
#
# 26G of a 32GiB VM leaves headroom for page cache and the system slice.
write_file "/etc/systemd/system/user@${TARGET_UID}.service.d/override.conf" <<'EOF'
# 2026-08-26 review-fleet thrash backstop. Bounds every process in the user
# manager, so an unwrapped agent launch cannot reach the VM ceiling.
[Service]
MemoryMax=26G
MemorySwapMax=2G
EOF
run systemctl daemon-reload
echo "  NOTE: MemoryMax applies to the user manager at its NEXT start."
echo "        It takes effect on reboot, or on: loginctl terminate-user $TARGET_USER"
echo "        (that kills the user's sessions, so do it deliberately)"

echo "==> 3/4  sshd hardening"
# This box is now an SSH-reachable agent host holding SSH keys, op secrets and
# kubeconfigs. Key-only auth from the Mac is the intended access path.
write_file /etc/ssh/sshd_config.d/99-herd-hardening.conf <<'EOF'
# 2026-08-26: this host became an SSH-reachable agent runner. Key-only auth.
PasswordAuthentication no
PermitRootLogin no
KbdInteractiveAuthentication no
EOF
if [ "$DRY" = "1" ]; then
  echo "  WOULD RUN: sshd -t && systemctl reload ssh"
else
  # Validate BEFORE reloading. A bad sshd config that gets reloaded can lock you
  # out of the only remote access path to this machine.
  if sshd -t 2>/dev/null; then
    systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || true
    echo "  sshd config valid; reloaded"
  else
    echo "  sshd -t FAILED; config written but NOT reloaded. Fix before reloading:" >&2
    sshd -t || true
  fi
fi
echo "  REMINDER: confirm the Windows firewall inbound rule for port 22 is"
echo "            scoped to your LAN/Tailscale subnet, not 'Any'."

echo "==> 4/4  Reduce listening surface: rpcbind"
if systemctl list-unit-files rpcbind.socket >/dev/null 2>&1; then
  run systemctl disable --now rpcbind.socket
  run systemctl disable --now rpcbind.service
  echo "  rpcbind: $(systemctl is-active rpcbind.socket 2>/dev/null || echo inactive)"
else
  echo "  rpcbind: not present (skipped)"
fi

echo
echo "==> Verify"
echo "  swappiness:   $(cat /proc/sys/vm/swappiness 2>/dev/null)"
echo "  oomd:         $(systemctl is-active systemd-oomd.service 2>/dev/null || echo n/a)"
echo "  user MemMax:  $(systemctl show "user@${TARGET_UID}.service" -p MemoryMax --value 2>/dev/null)"
echo "  MemTotal:     $(awk '/MemTotal/{printf "%.1fGiB", $2/1048576}' /proc/meminfo)"
echo
echo "Per-agent caps are already live and independent of this script:"
echo "  pid=\$(pgrep -f pool-NN); cg=\$(cut -d: -f3 /proc/\$pid/cgroup | head -1)"
echo "  cat /sys/fs/cgroup\$cg/memory.max     # expect 4294967296"
