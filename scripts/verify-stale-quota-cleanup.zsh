#!/usr/bin/env zsh
# Hosted failure-path controls for the quota driver's real checkout lifecycle.
set -euo pipefail
repo_root=$(git -C "${0:A:h}/.." rev-parse --show-toplevel)
report=${1:?report directory required}
mkdir -p -- "$report"
real_git=$commands[git]
fixture=$(mktemp -d)
fixture=$(cd "$fixture" && pwd -P)
trap 'rm -rf -- "$fixture" || exit 1' EXIT
trap 'exit 143' INT TERM HUP
mkdir -p "$fixture/repo/scripts" "$fixture/repo/pkg/usage" "$fixture/bin" "$fixture/tmp"
export TMPDIR=$fixture/tmp
export QUOTA_CLEANUP_REAL_GIT=$real_git QUOTA_CLEANUP_FIXTURE=$fixture
"$real_git" -C "$fixture/repo" init -q
"$real_git" -C "$fixture/repo" config user.name 'Cleanup fixture'
"$real_git" -C "$fixture/repo" config user.email 'cleanup@example.invalid'
"$real_git" -C "$fixture/repo" config core.hooksPath /dev/null
"$real_git" -C "$repo_root" show HEAD:pkg/usage/cache.go > "$fixture/repo/pkg/usage/cache.go"
"$real_git" -C "$fixture/repo" add pkg/usage/cache.go
"$real_git" -C "$fixture/repo" -c commit.gpgsign=false commit -qm fixture
"$real_git" -C "$fixture/repo" worktree add --detach "$fixture/other-registered" HEAD >/dev/null 2>&1
mkdir "$fixture/other-unregistered"
print -r -- keep > "$fixture/other-registered/sentinel"
print -r -- keep > "$fixture/other-unregistered/sentinel"
before=$("$real_git" -C "$fixture/repo" worktree list --porcelain -z)

driver=$(<"$repo_root/scripts/verify-stale-quota-error.zsh")
boundary='# Worktree creation is complete; verification starts below.'
parts=("${(@ps:$boundary:)driver}")
(( ${#parts} == 2 )) || { print -u2 'error: cleanup fixture boundary drifted'; exit 1; }
original=$parts[1]
armed=$'# Arm cleanup before Git can create or register the disposable checkout.\nowned=1\ntimeout -k 10s 120s git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1'
delayed=$'timeout -k 10s 120s git -C "$repo_root" worktree add --detach "$work" "$pin" >/dev/null 2>&1\nowned=1'
parts=("${(@ps:$armed:)original}")
(( ${#parts} == 2 )) || { print -u2 'error: ownership mutation anchor drifted'; exit 1; }
mutant=${original/"$armed"/"$delayed"}
[[ "$mutant" != "$original" ]] || exit 1

# Delegate every Git operation except the one exact disposable add request.
# The post-registration case performs the real add before forcing failure.
cat > "$fixture/bin/git" <<'WRAPPER'
#!/usr/bin/env zsh
set -euo pipefail
if (( $# == 7 )) && [[ "$1" == -C && "$2" == "$QUOTA_CLEANUP_FIXTURE/repo" && "$3" == worktree && "$4" == add && "$5" == --detach ]]; then
	checkout=$6
	[[ "${checkout:h:h}" == "$QUOTA_CLEANUP_FIXTURE/tmp" && "${checkout:t}" == checkout ]] || exit 91
	[[ ! -e "$QUOTA_CLEANUP_CASE/path" ]] || exit 92
	print -r -- "$checkout" > "$QUOTA_CLEANUP_CASE/path"
	if [[ "$QUOTA_CLEANUP_MODE" == registered ]]; then
		"$QUOTA_CLEANUP_REAL_GIT" "$@"
	else
		mkdir -- "$checkout"
		print -r -- partial > "$checkout/partial"
	fi
	print -r -- "$QUOTA_CLEANUP_MODE" > "$QUOTA_CLEANUP_CASE/forced"
	exit 73
fi
exec "$QUOTA_CLEANUP_REAL_GIT" "$@"
WRAPPER
chmod +x "$fixture/bin/git"

for variant in baseline mutant restored; do
	if [[ "$variant" == mutant ]]; then
		print -r -- "$mutant" > "$fixture/repo/scripts/driver.zsh"
	else
		print -r -- "$original" > "$fixture/repo/scripts/driver.zsh"
	fi
	for mode in registered unregistered; do
		case_dir=$fixture/$variant-$mode
		mkdir "$case_dir"
		rc=0
		PATH="$fixture/bin:$PATH" QUOTA_CLEANUP_CASE="$case_dir" QUOTA_CLEANUP_MODE="$mode" \
			VERIFY_STALE_QUOTA_REPORT_DIR="$case_dir/reports" \
			timeout -k 2s 15s zsh "$fixture/repo/scripts/driver.zsh" > "$case_dir/raw.log" 2>&1 || rc=$?
		log=$(<"$case_dir/raw.log")
		print -r -- "${log//"$fixture"/'<fixture>'}" > "$report/$variant-$mode.log"
		[[ -f "$case_dir/forced" && "$(<"$case_dir/forced")" == "$mode" ]] || { print -u2 'error: forced add failure was not reached'; exit 1; }
		if (( rc != 73 )) && [[ "$variant" != mutant || "$rc" != 1 ]]; then
			print -u2 "error: unexpected child exit $rc"
			exit 1
		fi
		checkout=$(<"$case_dir/path")
		after=$("$real_git" -C "$fixture/repo" worktree list --porcelain -z)
		clean=0
		[[ ! -e "${checkout:h}" && "$after" == "$before" ]] && clean=1
		if [[ "$variant" == mutant ]]; then
			(( clean == 0 )) || { print -u2 'error: delayed ownership mutant survived cleanup assertion'; exit 1; }
			[[ -d "$checkout" ]] || { print -u2 'error: mutant did not leave the intended checkout'; exit 1; }
			if [[ "$mode" == registered ]]; then
				"$real_git" -C "$fixture/repo" worktree remove --force "$checkout"
			else
				rm -rf -- "$checkout"
			fi
			rmdir -- "${checkout:h}"
			print -r -- "KILLED: delayed ownership: $mode: disposable checkout leaked" >> "$report/summary.txt"
		else
			(( clean == 1 )) || { print -u2 'error: disposable checkout leaked'; exit 1; }
			print -r -- "PASS: $variant: $mode: disposable checkout removed" >> "$report/summary.txt"
		fi
		[[ "$(<"$fixture/other-registered/sentinel")" == keep && "$(<"$fixture/other-unregistered/sentinel")" == keep ]] || { print -u2 'error: cleanup changed another checkout'; exit 1; }
		[[ "$("$real_git" -C "$fixture/repo" worktree list --porcelain -z)" == "$before" ]] || { print -u2 'error: cleanup changed another registration'; exit 1; }
	done
done
