#!/usr/bin/env zsh
# FAC-486: pre-compile Go toolchain guard.
#
# pkg/preflight.CheckGoToolchain already diagnoses this condition correctly, but
# it is Go code — reaching it requires compiling Go, which is precisely what a
# GOROOT mismatch breaks. Every Make entry point therefore died with the
# compiler-internal "compile: version ... does not match go tool version ..."
# and the guard never ran. This script is the same check at the only layer that
# runs before the compiler does. Keep the two diagnostics consistent.
set -euo pipefail

# Not exported is the normal case: the go driver picks its own GOROOT.
[[ -n "${GOROOT:-}" ]] || exit 0

if ! path_goroot="$(env -u GOROOT go env GOROOT 2>&1)"; then
  print -u2 "Go toolchain check: cannot resolve the PATH-resolved GOROOT while GOROOT=\"$GOROOT\" is exported: $path_goroot"
  exit 1
fi

# :A resolves symlinks and normalises, so a symlinked-but-identical root passes.
[[ "${GOROOT:A}" == "${path_goroot:A}" ]] && exit 0

# $GOROOT/VERSION is "go1.26.2\ntime <stamp>"; take the leading word only.
goroot_version="version unavailable"
if [[ -r "$GOROOT/VERSION" ]]; then
  goroot_version="${${(z)"$(<"$GOROOT/VERSION")"}[1]:-version unavailable}"
fi

# `go version` is "go version go1.26.6 darwin/arm64" — the release is word 3.
path_version="version unavailable"
if version_output="$(env -u GOROOT go version 2>/dev/null)"; then
  version_word="${${(z)version_output}[3]:-}"
  [[ "$version_word" == go* ]] && path_version="$version_word"
fi

print -u2 "Go toolchain mismatch: exported GOROOT=\"$GOROOT\" ($goroot_version), but PATH-resolved go uses GOROOT=\"$path_goroot\" ($path_version); unset GOROOT (env -u GOROOT make build) and retry"
exit 1
