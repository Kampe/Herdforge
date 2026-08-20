#!/bin/zsh

# Build herd without ever truncating an executable that a live lane may be
# running. The temporary output and both final paths are on the repository
# filesystem, so each rename is atomic for readers.
set -euo pipefail

root=${1:-$(pwd -P)}
root=$(cd "$root" && pwd -P)
top=$(git -C "$root" rev-parse --show-toplevel 2>/dev/null) || {
  print -u2 "build refused: Git toplevel does not resolve from the build directory"
  exit 1
}
if [[ "$top" != "$root" ]]; then
  print -u2 "build refused: Git toplevel $top does not match build directory $root"
  exit 1
fi

mkdir -p "$root/bin"
rev=$(git -C "$root" rev-parse HEAD)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
tmp=$(mktemp "$root/bin/.herd.XXXXXX")
link=$(mktemp "$root/.herd-link.XXXXXX")
cleanup() { rm -f "$tmp" "$link" }
trap cleanup EXIT INT TERM

go_cmd=${HERD_GO:-go}
"$go_cmd" build \
  -ldflags "-X github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision=$rev -X github.com/Kampe/Herdforge/pkg/provenance.BinaryBuildTime=$now" \
  -o "$tmp" ./cmd/herd

if ! "$tmp" --version | grep -F "revision $rev" >/dev/null; then
  print -u2 "build refused: temporary herd failed provenance check"
  exit 1
fi

# Rename only after build and smoke validation succeed. Existing consumers keep
# their old inode; new execs see either the old or complete new executable.
mv -f "$tmp" "$root/bin/herd"
rm -f "$link"
ln -s "bin/herd" "$link"
mv -f "$link" "$root/herd"
