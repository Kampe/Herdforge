#!/usr/bin/env zsh
# Scan every tracked Go module for HIGH and CRITICAL vulnerabilities.
#
# The module list comes from Git rather than a filesystem walk so generated
# herd state and sibling worktrees cannot affect either scope or results.
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

typeset -a tracked_modules
tracked_modules=("${(@f)$(
	git ls-files -z | while IFS= read -r -d '' path; do
		case "$path" in
			go.mod|*/go.mod)
				case "/$path/" in
					*/.worktrees/*|*/.herd/*) ;;
					*) print -r -- "${path:h}" ;;
				esac
				;;
		esac
	done | LC_ALL=C sort -u
)}")

if (( ${#tracked_modules[@]} == 0 )); then
	print -u2 -- "error: no tracked Go modules found"
	exit 1
fi

print -- "==> Scanning tracked Go modules for HIGH/CRITICAL vulnerabilities..."
for module in "${tracked_modules[@]}"; do
	print -- "--- $module ---"
	trivy fs --scanners vuln --severity HIGH,CRITICAL --exit-code 1 --no-progress \
		--skip-dirs .worktrees --skip-dirs .herd "$module"
done
