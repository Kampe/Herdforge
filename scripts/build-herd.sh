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

# FAC-717: bin/herdforge is a SYMLINK, never a second executable.
#
# docs/operations/second-host-wsl.md tells operators to run `./bin/herdforge
# preflight`, and nothing in this build ever produced that file. A copy left
# over from an earlier build sat there for SIX DAYS reporting a stale revision,
# and a lane pulsing it read working=1 capacity=14 against a live fleet of
# working=9 capacity=6. Every census in that lane's reports came from code six
# days behind the tree, and the provenance line said UNKNOWN rather than saying
# the readings could not be trusted.
#
# A second executable can drift. A symlink cannot: it resolves to whatever
# bin/herd currently is, so the documented entrypoint and the built one are the
# same file by construction rather than by remembering to rebuild both.
rm -f "$root/bin/herdforge"
ln -s "herd" "$root/bin/herdforge"
