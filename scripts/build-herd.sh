#!/bin/sh

# Build herd without ever truncating an executable that a live lane may be
# running. The temporary output and both final paths are on the repository
# filesystem, so each rename is atomic for readers.
#
# FAC-542: POSIX sh, not zsh. This script is invoked by `make build`, which
# also runs inside CI containers that have no zsh — there it died with
# `./scripts/build-herd.zsh: not found` (exit 127), failing the hermetic
# profile before any build happened.
set -eu

root=${1:-$(pwd -P)}
root=$(cd "$root" && pwd -P)
if ! top=$(git -C "$root" rev-parse --show-toplevel 2>/dev/null); then
  printf '%s\n' "build refused: Git toplevel does not resolve from the build directory" >&2
  exit 1
fi
if [ "$top" != "$root" ]; then
  printf '%s\n' "build refused: Git toplevel $top does not match build directory $root" >&2
  exit 1
fi

mkdir -p "$root/bin"
rev=$(git -C "$root" rev-parse HEAD)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
tmp=$(mktemp "$root/bin/.herd.XXXXXX")
link=$(mktemp "$root/.herd-link.XXXXXX")
cleanup() { rm -f "$tmp" "$link"; }
trap cleanup EXIT INT TERM

go_cmd=${HERD_GO:-go}
"$go_cmd" build \
  -ldflags "-X github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision=$rev -X github.com/Kampe/Herdforge/pkg/provenance.BinaryBuildTime=$now" \
  -o "$tmp" ./cmd/herd

if ! "$tmp" --version | grep -F "revision $rev" >/dev/null; then
  printf '%s\n' "build refused: temporary herd failed provenance check" >&2
  exit 1
fi

# Rename only after build and smoke validation succeed. Existing consumers keep
# their old inode; new execs see either the old or complete new executable.
mv -f "$tmp" "$root/bin/herd"
rm -f "$link"
ln -s "bin/herd" "$link"
mv -f "$link" "$root/herd"
