#!/bin/sh
# FAC-486: pre-compile Go toolchain guard.
#
# pkg/preflight.CheckGoToolchain already diagnoses this condition correctly, but
# it is Go code — reaching it requires compiling Go, which is precisely what a
# GOROOT mismatch breaks. Every Make entry point therefore died with the
# compiler-internal "compile: version ... does not match go tool version ..."
# and the guard never ran. This script is the same check at the only layer that
# runs before the compiler does. Keep the two diagnostics consistent.
#
# FAC-541: POSIX sh, not zsh. This gate runs inside CI containers (the hermetic
# Docker profile and the coverage job) that have no zsh, where the zsh version
# died with `/usr/bin/env: 'zsh': No such file or directory` (exit 127) and took
# every Make target with it. A guard that cannot run in the environments it
# guards is worse than no guard.
set -eu

# Not exported is the normal case: the go driver picks its own GOROOT.
[ -n "${GOROOT:-}" ] || exit 0

if ! path_goroot="$(env -u GOROOT go env GOROOT 2>&1)"; then
  printf '%s\n' "Go toolchain check: cannot resolve the PATH-resolved GOROOT while GOROOT=\"$GOROOT\" is exported: $path_goroot" >&2
  exit 1
fi

# Resolve symlinks so a symlinked-but-identical root passes. `cd -P` is the
# portable equivalent of zsh's :A modifier.
resolve() {
  [ -d "$1" ] || { printf '%s\n' "$1"; return; }
  ( cd -P -- "$1" 2>/dev/null && pwd -P ) || printf '%s\n' "$1"
}
[ "$(resolve "$GOROOT")" = "$(resolve "$path_goroot")" ] && exit 0

# $GOROOT/VERSION is "go1.26.2\ntime <stamp>"; take the leading word only.
goroot_version="version unavailable"
if [ -r "$GOROOT/VERSION" ]; then
  read_version="$(head -n 1 "$GOROOT/VERSION" 2>/dev/null | awk '{print $1}')"
  [ -n "$read_version" ] && goroot_version="$read_version"
fi

# `go version` is "go version go1.26.6 darwin/arm64" — the release is word 3.
path_version="version unavailable"
if version_output="$(env -u GOROOT go version 2>/dev/null)"; then
  version_word="$(printf '%s\n' "$version_output" | awk '{print $3}')"
  case "$version_word" in go*) path_version="$version_word" ;; esac
fi

printf '%s\n' "Go toolchain mismatch: exported GOROOT=\"$GOROOT\" ($goroot_version), but PATH-resolved go uses GOROOT=\"$path_goroot\" ($path_version); unset GOROOT (env -u GOROOT make build) and retry" >&2
exit 1
