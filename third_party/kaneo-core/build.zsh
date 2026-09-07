#!/bin/zsh
# Build the pinned, reviewed dependency; retain source for audit and safe cleanup.
set -euo pipefail
project_root=${0:A:h:h:h}
input_dir="$project_root/third_party/kaneo-core"
base=f47a39dbae091c6842b5f46a788f083d21782cc8
mkdir -p "$project_root/.herd/toolchains" "$project_root/bin"
source_dir=$(mktemp -d "$project_root/.herd/toolchains/kaneo-core.XXXXXX")
git clone --quiet --no-checkout https://github.com/onreza/kaneo-cli.git "$source_dir/source"
git -C "$source_dir/source" checkout --quiet --detach "$base"
[[ $(git -C "$source_dir/source" rev-parse HEAD) == "$base" ]]
git -C "$source_dir/source" apply --check "$input_dir/core-read.patch"
git -C "$source_dir/source" apply "$input_dir/core-read.patch"
cd "$source_dir/source"
cargo build --locked
cargo test --locked
python3 tests/core_read.py
cargo build --locked --release
# Publish only after all build and fixture gates passed. Global kaneo is untouched.
cp target/release/kaneo "$source_dir/kaneo-core"
chmod 0755 "$source_dir/kaneo-core"
mv "$source_dir/kaneo-core" "$project_root/bin/kaneo-core"
