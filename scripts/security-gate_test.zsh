#!/usr/bin/env zsh
# FAC-251 non-vacuity checks for the deterministic tracked-source gate.
set -euo pipefail
root=$(git rev-parse --show-toplevel)
tmp=$(mktemp -d)
cleanup() { find "$tmp" -depth -delete 2>/dev/null || true; }
trap cleanup EXIT
mkdir "$tmp/repo"
cd "$tmp/repo"
git init -q
git config user.email fac251@example.invalid
git config user.name fac251
git config commit.gpgsign false
git config core.hooksPath /dev/null
# This is a new, minimal tracked Go repository. Its Gitleaks baseline is
# intentionally empty; production history belongs only to the production
# baseline and must not make this non-vacuity fixture pass by accident.
mkdir -p scripts security/baselines
cp -p "$root/scripts/security-gate.zsh" scripts/security-gate.zsh
print 'module example.invalid/fac251fixture' > go.mod
print 'package fixture' > fixture.go
print '# rule\tfile\tline\tfingerprint\trationale\towner\texpiry' > security/baselines/gosec-high.tsv
git add -A
git -c commit.gpgsign=false commit -qm 'test: current FAC-251 gate fixture'
print '# fingerprint\tclassification\towner\texpiry' > security/baselines/gitleaks.tsv
mkdir -p .worktrees/live
# Proven by a disposable `gitleaks git` fixture: the assembled value matches
# the detector rule, but no token-shaped literal is committed with this test harness.
token_prefix=$'\x67\x68\x70\x5f'
token_suffix=""
for (( i=0; i<36; i++ )); do
	token_suffix+=$(( i % 10 ))
done
token_name="git"
token_name+="hub"
token_name+="_"
token_name+="token"
print -- "${token_name} = \"${token_prefix}${token_suffix}\"" > .worktrees/live/secret.txt
# --- Hermetic Mock Scanner & Timeout Tests (no installed scanner dependencies) ---
mock_bin="$tmp/mock-bin"
mkdir -p "$mock_bin"

# 1. Test gosec timeout enforcement and child process cleanup
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
if [[ -n "${MOCK_PID_FILE-}" ]]; then
	print "$$" > "$MOCK_PID_FILE"
fi
exec sleep 30
EOF
chmod +x "$mock_bin/gosec"

cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
for ((i=1; i<=$#; i++)); do
	if [[ "${@[i]}" == "--report-path" ]]; then
		print '[]' > "${@[i+1]}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gitleaks"

gosec_timeout_out="$tmp/gosec-timeout.out"
mock_gosec_pid="$tmp/mock-gosec.pid"
if MOCK_PID_FILE="$mock_gosec_pid" PATH="$mock_bin:$PATH" GOSEC_TIMEOUT=1 ./scripts/security-gate.zsh >"$gosec_timeout_out" 2>&1; then
	print -u2 "error: security-gate did not fail closed on gosec timeout"
	exit 1
fi
grep -F -- "error: gosec timed out after 1s" "$gosec_timeout_out" >/dev/null || {
	print -u2 "error: missing gosec timeout diagnostic in output"
	exit 1
}
if [[ -f "$mock_gosec_pid" ]]; then
	killed_pid=$(cat "$mock_gosec_pid")
	if kill -0 "$killed_pid" 2>/dev/null; then
		print -u2 "error: timed out gosec process $killed_pid was not killed by cleanup"
		kill -9 "$killed_pid" 2>/dev/null || true
		exit 1
	fi
fi

# 2. Test gitleaks timeout enforcement and child process cleanup
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"Issues":[]}' > "${arg#-out=}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"

cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
if [[ -n "${MOCK_PID_FILE-}" ]]; then
	print "$$" > "$MOCK_PID_FILE"
fi
exec sleep 30
EOF
chmod +x "$mock_bin/gitleaks"

gitleaks_timeout_out="$tmp/gitleaks-timeout.out"
mock_gitleaks_pid="$tmp/mock-gitleaks.pid"
if MOCK_PID_FILE="$mock_gitleaks_pid" PATH="$mock_bin:$PATH" GITLEAKS_TIMEOUT=1 ./scripts/security-gate.zsh >"$gitleaks_timeout_out" 2>&1; then
	print -u2 "error: security-gate did not fail closed on gitleaks timeout"
	exit 1
fi
grep -F -- "error: gitleaks timed out after 1s" "$gitleaks_timeout_out" >/dev/null || {
	print -u2 "error: missing gitleaks timeout diagnostic in output"
	exit 1
}
if [[ -f "$mock_gitleaks_pid" ]]; then
	killed_pid=$(cat "$mock_gitleaks_pid")
	if kill -0 "$killed_pid" 2>/dev/null; then
		print -u2 "error: timed out gitleaks process $killed_pid was not killed by cleanup"
		kill -9 "$killed_pid" 2>/dev/null || true
		exit 1
	fi
fi

# 3. Test mock success within timeout
cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
for ((i=1; i<=$#; i++)); do
	if [[ "${@[i]}" == "--report-path" ]]; then
		print '[]' > "${@[i+1]}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gitleaks"

PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >/dev/null

# 4. Stale gosec baseline detection under mock scanner
stale_fp1=$(print -rn -- 'G702|fixture.go|999' | shasum -a 256 | awk '{print $1}')
stale_fp2=$(print -rn -- 'G703|fixture.go|888' | shasum -a 256 | awk '{print $1}')
print -- "G702\tfixture.go\t999\t$stale_fp1\tfixture stale-entry regression 1\tsecurity-maintainers\t2026-12-31" >> security/baselines/gosec-high.tsv
print -- "G703\tfixture.go\t888\t$stale_fp2\tfixture stale-entry regression 2\tsecurity-maintainers\t2026-12-31" >> security/baselines/gosec-high.tsv
stale_report="$tmp/stale-gosec.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$stale_report" 2>&1; then exit 1; fi
grep -F -- "error: stale baseline entry $stale_fp1" "$stale_report" >/dev/null || exit 1
grep -F -- "error: stale baseline entry $stale_fp2" "$stale_report" >/dev/null || exit 1
print '# rule\tfile\tline\tfingerprint\trationale\towner\texpiry' > security/baselines/gosec-high.tsv
PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >/dev/null

# 5. Secret detection & expired/missing baseline tests under mock scanner
print -- "${token_name} = \"${token_prefix}${token_suffix}\"" > tracked-secret.txt
git add tracked-secret.txt
git -c commit.gpgsign=false commit -qm 'test: introduce detected secret'

mock_leak_fp=$(print -rn -- "mock-leak-fingerprint" | shasum -a 256 | awk '{print $1}')
cat << EOF > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
for ((i=1; i<=\$#; i++)); do
	if [[ "\${@[i]}" == "--report-path" ]]; then
		print '[{"Fingerprint":"$mock_leak_fp","RuleID":"github-pat","File":"tracked-secret.txt","StartLine":1}]' > "\${@[i+1]}"
	fi
done
exit 1
EOF
chmod +x "$mock_bin/gitleaks"

print '# fingerprint\tclassification\towner\texpiry' > "$tmp/expired.tsv"
print -- "$mock_leak_fp\tfixture\tsecurity-maintainers\t2000-01-01" >> "$tmp/expired.tsv"
if GITLEAKS_BASELINE="$tmp/expired.tsv" PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi
if GITLEAKS_BASELINE="$tmp/missing.tsv" PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi

# Valid baseline matches mock leak finding
print '# fingerprint\tclassification\towner\texpiry' > "$tmp/valid.tsv"
print -- "$mock_leak_fp\tfixture\tsecurity-maintainers\t2026-12-31" >> "$tmp/valid.tsv"
GITLEAKS_BASELINE="$tmp/valid.tsv" PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >/dev/null

# Unreviewed leak finding fails closed
print '# fingerprint\tclassification\towner\texpiry' > "$tmp/empty.tsv"
if GITLEAKS_BASELINE="$tmp/empty.tsv" PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi

# Optional integration tests with real scanners when present in environment
if (( $+commands[gosec] && $+commands[gitleaks] )); then
	fixture_report="$tmp/real-gitleaks.json"
	gitleaks git --no-banner --redact=100 --report-format json --report-path "$fixture_report" . >/dev/null 2>&1 || true
	real_fp=$(jq -r '.[].Fingerprint' "$fixture_report" 2>/dev/null | LC_ALL=C sort -u | head -n 1)
	if [[ -n "$real_fp" && "$real_fp" != null ]]; then
		print '# fingerprint\tclassification\towner\texpiry' > "$tmp/real-expired.tsv"
		print -- "$real_fp\tfixture\tsecurity-maintainers\t2000-01-01" >> "$tmp/real-expired.tsv"
		if GITLEAKS_BASELINE="$tmp/real-expired.tsv" ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi
		print '# fingerprint\tclassification\towner\texpiry' > "$tmp/real-valid.tsv"
		print -- "$real_fp\tfixture\tsecurity-maintainers\t2026-12-31" >> "$tmp/real-valid.tsv"
		GITLEAKS_BASELINE="$tmp/real-valid.tsv" ./scripts/security-gate.zsh >/dev/null
		# Default baseline has no entries, so unreviewed history leak must fail closed
		if ./scripts/security-gate.zsh >/dev/null 2>&1; then exit 1; fi
	fi
fi

print '==> FAC-251 security negative tests passed'
