#!/usr/bin/env zsh
# Scan every tracked Go module for HIGH and CRITICAL vulnerabilities.
#
# The module list comes from Git rather than a filesystem walk so generated
# herd state and sibling worktrees cannot affect either scope or results.
#
# FAC-755: enumerate with a NUL-split array (same form as security-gate.zsh),
# not `git ls-files -z | while read`. A pipeline under a background process
# group of a controlling terminal can stop (SIGTTIN/SIGTTOU) and hang make lint.
set -euo pipefail

if (( $+commands[git] == 0 )); then
	print -u2 -- "error: git is required to enumerate tracked Go modules"
	exit 1
fi

if (( $+commands[trivy] == 0 )); then
	print -u2 -- "error: trivy is required; install it before running this gate"
	exit 1
fi

repo_root=$(git rev-parse --show-toplevel) || exit 1
cd "$repo_root"

typeset -a tracked_files
tracked_files=( ${(0)"$(git ls-files -z)"} )

typeset -a tracked_modules
for file_path in "${tracked_files[@]}"; do
	[[ -z "$file_path" ]] && continue
	case "$file_path" in
		go.mod|*/go.mod)
			case "/$file_path/" in
				*/.worktrees/*|*/.herd/*) ;;
				*) tracked_modules+=( "${file_path:h}" ) ;;
			esac
			;;
	esac
done

if (( ${#tracked_modules[@]} == 0 )); then
	print -u2 -- "error: no tracked Go modules found"
	exit 1
fi

() {
	local LC_ALL=C
	tracked_modules=( ${(ou)tracked_modules} )
}

print -- "==> Scanning tracked Go modules for HIGH/CRITICAL vulnerabilities..."
for module in "${tracked_modules[@]}"; do
	print -- "--- $module ---"
	trivy fs --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress \
		--skip-dirs .worktrees --skip-dirs .herd "$module"
done
