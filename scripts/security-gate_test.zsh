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
# An explicit environment override bypasses the surface derivation entirely:
# no derived-budget diagnostic may appear on an overridden run.
if grep -F -- "gosec budget" "$gosec_timeout_out" >/dev/null 2>&1; then
	print -u2 "error: gosec budget was derived despite an explicit GOSEC_TIMEOUT override"
	exit 1
fi

# Test SECURITY_GATE_TIMEOUT fallback for gosec
gosec_shared_timeout_out="$tmp/gosec-shared-timeout.out"
if PATH="$mock_bin:$PATH" SECURITY_GATE_TIMEOUT=1 ./scripts/security-gate.zsh >"$gosec_shared_timeout_out" 2>&1; then
	print -u2 "error: security-gate did not fail closed on shared SECURITY_GATE_TIMEOUT for gosec"
	exit 1
fi
grep -F -- "error: gosec timed out after 1s" "$gosec_shared_timeout_out" >/dev/null || {
	print -u2 "error: missing gosec shared timeout diagnostic in output"
	exit 1
}

# Test GOSEC_TIMEOUT precedence over SECURITY_GATE_TIMEOUT
gosec_precedence_out="$tmp/gosec-precedence.out"
if PATH="$mock_bin:$PATH" GOSEC_TIMEOUT=1 SECURITY_GATE_TIMEOUT=99 ./scripts/security-gate.zsh >"$gosec_precedence_out" 2>&1; then
	print -u2 "error: security-gate did not respect GOSEC_TIMEOUT precedence"
	exit 1
fi
grep -F -- "error: gosec timed out after 1s" "$gosec_precedence_out" >/dev/null || {
	print -u2 "error: missing gosec precedence timeout diagnostic"
	exit 1
}

# An INVALID override must fail closed with a diagnostic, never silently
# degrade to a default (or a derived/unbounded) budget.
invalid_out="$tmp/gosec-invalid-override.out"
if GOSEC_TIMEOUT=abc PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$invalid_out" 2>&1; then
	print -u2 "error: invalid GOSEC_TIMEOUT was accepted instead of failing closed"
	exit 1
fi
grep -F -- 'error: GOSEC_TIMEOUT must be a positive integer number of seconds' "$invalid_out" >/dev/null || {
	print -u2 "error: missing invalid GOSEC_TIMEOUT diagnostic"
	exit 1
}
if SECURITY_GATE_TIMEOUT=abc PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$invalid_out" 2>&1; then
	print -u2 "error: invalid SECURITY_GATE_TIMEOUT was accepted instead of failing closed"
	exit 1
fi
grep -F -- 'error: SECURITY_GATE_TIMEOUT must be a positive integer number of seconds' "$invalid_out" >/dev/null || {
	print -u2 "error: missing invalid SECURITY_GATE_TIMEOUT diagnostic"
	exit 1
}
if GITLEAKS_TIMEOUT=0 PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$invalid_out" 2>&1; then
	print -u2 "error: zero GITLEAKS_TIMEOUT was accepted instead of failing closed"
	exit 1
fi
grep -F -- 'error: GITLEAKS_TIMEOUT must be a positive integer number of seconds' "$invalid_out" >/dev/null || {
	print -u2 "error: missing invalid GITLEAKS_TIMEOUT diagnostic"
	exit 1
}

# A gosec crash that leaves an EMPTY subreport must fail closed with a
# diagnostic naming gosec's report, not pass with zero findings, and not
# surface as an unrelated jq iteration error.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		: > "${arg#-out=}"
	fi
done
exit 2
EOF
chmod +x "$mock_bin/gosec"
gosec_crash_empty_out="$tmp/gosec-crash-empty.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_crash_empty_out" 2>&1; then
	print -u2 "error: empty crashed gosec report was accepted"
	exit 1
fi
grep -F -- 'error: gosec produced no complete single-document JSON report' "$gosec_crash_empty_out" >/dev/null || {
	print -u2 "error: missing empty gosec report diagnostic"
	exit 1
}

# A subreport WITHOUT an Issues key must fail closed: jq reads a missing
# member as null, so only the pinned gosec shape (an object that HAS the
# Issues member) may pass.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{}' > "${arg#-out=}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"
gosec_missing_key_out="$tmp/gosec-missing-key.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_missing_key_out" 2>&1; then
	print -u2 "error: gosec report without an Issues key was accepted"
	exit 1
fi
grep -F -- 'error: gosec produced no complete single-document JSON report' "$gosec_missing_key_out" >/dev/null || {
	print -u2 "error: missing gosec missing-key diagnostic"
	exit 1
}

# Error-shaped bodies must fail closed even when they also supply
# Issues: null or Issues: [].
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"Err":"gosec panicked: stack overflow","Issues":null}' > "${arg#-out=}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"
gosec_err_body_out="$tmp/gosec-err-body.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_err_body_out" 2>&1; then
	print -u2 "error: gosec Err body with Issues null was accepted"
	exit 1
fi
grep -F -- 'error: gosec produced no complete single-document JSON report' "$gosec_err_body_out" >/dev/null || {
	print -u2 "error: missing gosec error-body diagnostic"
	exit 1
}
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"error":"exit status 2","Issues":[]}' > "${arg#-out=}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"
gosec_error_body_out="$tmp/gosec-error-body.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_error_body_out" 2>&1; then
	print -u2 "error: gosec error body with Issues array was accepted"
	exit 1
fi
grep -F -- 'error: gosec produced no complete single-document JSON report' "$gosec_error_body_out" >/dev/null || {
	print -u2 "error: missing gosec error-body (lowercase) diagnostic"
	exit 1
}

# A gosec crash that leaves a MALFORMED subreport must fail closed; the
# aggregation must not swallow the parse failure and evaluate zero findings.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print 'garbage{' > "${arg#-out=}"
	fi
done
exit 2
EOF
chmod +x "$mock_bin/gosec"
gosec_crash_malformed_out="$tmp/gosec-crash-malformed.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_crash_malformed_out" 2>&1; then
	print -u2 "error: malformed gosec report was accepted"
	exit 1
fi
grep -F -- 'error: gosec produced no complete single-document JSON report' "$gosec_crash_malformed_out" >/dev/null || {
	print -u2 "error: missing malformed gosec report diagnostic"
	exit 1
}


# A multi-document subreport is not one complete JSON document and must
# fail closed even though every document is individually valid.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"Issues":[]}{"Issues":[]}' > "${arg#-out=}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"
gosec_multidoc_out="$tmp/gosec-multidoc.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_multidoc_out" 2>&1; then
	print -u2 "error: multi-document gosec report was accepted"
	exit 1
fi
grep -F -- 'error: gosec produced no complete single-document JSON report' "$gosec_multidoc_out" >/dev/null || {
	print -u2 "error: missing multi-document gosec report diagnostic"
	exit 1
}

# The pinned gosec v2.22.10 emits, for no findings, a JSON object whose
# "Issues" member is null. That representation must be ACCEPTED and the
# gate must proceed (a bare top-level null remains the Gitleaks contract
# and is rejected; see the malformed/multidoc cases above).
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"Issues":null}' > "${arg#-out=}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"
gosec_issues_null_out="$tmp/gosec-issues-null.out"
if ! PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_issues_null_out" 2>&1; then
	print -u2 "error: pinned gosec Issues-null no-findings report was rejected"
	exit 1
fi
grep -F -- '==> FAC-251 gosec HIGH/CRITICAL baseline is exact and current' "$gosec_issues_null_out" >/dev/null || {
	print -u2 "error: missing gosec baseline diagnostic for Issues-null report"
	exit 1
}
# The gosec budget must derive from the measured scan surface. The fixture
# tracks exactly one shipped Go package (fixture.go), so the 300s floor holds
# and the derivation diagnostic must name BOTH values exactly — a wrong
# formula, a lost floor, or a lost cap fails these exact-value assertions.
grep -F -- '==> gosec budget 300s derived from 1 scanned Go packages' "$gosec_issues_null_out" >/dev/null || {
	print -u2 "error: gosec budget derivation missing or wrong at fixture scale"
	exit 1
}

# A genuine HIGH finding with a matching baseline row must pass the
# baseline-exactness check end to end.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		out="${arg#-out=}"
		scan_root=$(dirname "$(dirname "$out")")
		print "{\"Issues\":[{\"severity\":\"HIGH\",\"rule_id\":\"G104\",\"file\":\"$scan_root/fixture.go\",\"line\":3,\"what\":\"fixture finding\"}]}" > "$out"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gosec"
finding_fp=$(print -rn -- 'G104|fixture.go|3' | shasum -a 256 | awk '{print $1}')
print "# rule\tfile\tline\tfingerprint\trationale\towner\texpiry" > security/baselines/gosec-high.tsv
print "G104\tfixture.go\t3\t$finding_fp\tfixture reviewed finding\towner\t2099-12-31" >> security/baselines/gosec-high.tsv
git add security/baselines/gosec-high.tsv
git -c commit.gpgsign=false commit -qm 'test: fixture baseline for the genuine HIGH finding'
gosec_baseline_out="$tmp/gosec-baseline.out"
if ! PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_baseline_out" 2>&1; then
	print -u2 "error: genuine HIGH finding with exact baseline was rejected"
	exit 1
fi
grep -F -- '==> FAC-251 gosec HIGH/CRITICAL baseline is exact and current' "$gosec_baseline_out" >/dev/null || {
	print -u2 "error: missing baseline-exact diagnostic for genuine finding"
	exit 1
}

# Restore the empty baseline so later sections see the fixture's original
# no-findings state.
print '# rule\tfile\tline\tfingerprint\trationale\towner\texpiry' > security/baselines/gosec-high.tsv
git add security/baselines/gosec-high.tsv
git -c commit.gpgsign=false commit -qm 'test: restore empty FAC-251 fixture baseline'

# An operational gosec failure (exit status 2) with a COMPLETE report must
# fail closed: a status other than 0 or 124 is a scanner failure even when
# the report looks usable.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"Issues":[]}' > "${arg#-out=}"
	fi
done
exit 2
EOF
chmod +x "$mock_bin/gosec"
gosec_status2_out="$tmp/gosec-status2.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$gosec_status2_out" 2>&1; then
	print -u2 "error: gosec operational failure with complete report was accepted"
	exit 1
fi
grep -F -- 'error: gosec scanner failed with exit status 2' "$gosec_status2_out" >/dev/null || {
	print -u2 "error: missing gosec scanner failure diagnostic"
	exit 1
}

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

# Test SECURITY_GATE_TIMEOUT fallback for gitleaks
gitleaks_shared_timeout_out="$tmp/gitleaks-shared-timeout.out"
if PATH="$mock_bin:$PATH" SECURITY_GATE_TIMEOUT=1 ./scripts/security-gate.zsh >"$gitleaks_shared_timeout_out" 2>&1; then
	print -u2 "error: security-gate did not fail closed on shared SECURITY_GATE_TIMEOUT for gitleaks"
	exit 1
fi
grep -F -- "error: gitleaks timed out after 1s" "$gitleaks_shared_timeout_out" >/dev/null || {
	print -u2 "error: missing gitleaks shared timeout diagnostic in output"
	exit 1
}

# Test GITLEAKS_TIMEOUT precedence over SECURITY_GATE_TIMEOUT
gitleaks_precedence_out="$tmp/gitleaks-precedence.out"
if PATH="$mock_bin:$PATH" GITLEAKS_TIMEOUT=1 SECURITY_GATE_TIMEOUT=99 ./scripts/security-gate.zsh >"$gitleaks_precedence_out" 2>&1; then
	print -u2 "error: security-gate did not respect GITLEAKS_TIMEOUT precedence"
	exit 1
fi
grep -F -- "error: gitleaks timed out after 1s" "$gitleaks_precedence_out" >/dev/null || {
	print -u2 "error: missing gitleaks precedence timeout diagnostic in output"
	exit 1
}

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

# FAC-660 compatibility: a complete JSON null is a valid no-finding report.
cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
for ((i=1; i<=$#; i++)); do
	if [[ "${@[i]}" == "--report-path" ]]; then
		print 'null' > "${@[i+1]}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gitleaks"
null_report_out="$tmp/gitleaks-null.out"
if ! PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$null_report_out" 2>&1; then
	print -u2 "error: FAC-660 JSON null no-finding report was rejected"
	exit 1
fi

# A null no-finding report is inconsistent with a findings exit and must not
# be promoted to a successful scan.
sed -i '' 's/^exit 0$/exit 1/' "$mock_bin/gitleaks"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$null_report_out" 2>&1; then
	print -u2 "error: gitleaks exit1 with JSON null report was accepted"
	exit 1
fi
grep -F -- 'error: gitleaks returned 1 without findings' "$null_report_out" >/dev/null || {
	print -u2 "error: missing JSON null exit consistency diagnostic"
	exit 1
}

# Operational exit2 with an empty report must fail closed even when the
# baseline contains only unreachable/empty entries.
cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
exit 2
EOF
chmod +x "$mock_bin/gitleaks"
exit2_empty_out="$tmp/gitleaks-exit2-empty.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$exit2_empty_out" 2>&1; then
	print -u2 "error: empty exit2 gitleaks report was accepted"
	exit 1
fi
grep -F -- 'error: gitleaks scanner failed with exit status 2' "$exit2_empty_out" >/dev/null || {
	print -u2 "error: missing gitleaks exit2 diagnostic"
	exit 1
}

cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
for ((i=1; i<=$#; i++)); do
	if [[ "${@[i]}" == "--report-path" ]]; then
		print '[]' > "${@[i+1]}"
	fi
done
exit 1
EOF
chmod +x "$mock_bin/gitleaks"
exit1_empty_out="$tmp/gitleaks-exit1-empty.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$exit1_empty_out" 2>&1; then
	print -u2 "error: empty exit1 gitleaks report was accepted"
	exit 1
fi
grep -F -- 'error: gitleaks returned 1 without findings' "$exit1_empty_out" >/dev/null || {
	print -u2 "error: missing gitleaks exit1 consistency diagnostic"
	exit 1
}

cat << 'EOF' > "$mock_bin/gitleaks"
#!/usr/bin/env zsh
for ((i=1; i<=$#; i++)); do
	if [[ "${@[i]}" == "--report-path" ]]; then
		print '{"not":"an array"}' > "${@[i+1]}"
	fi
done
exit 0
EOF
chmod +x "$mock_bin/gitleaks"
malformed_out="$tmp/gitleaks-malformed.out"
if PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$malformed_out" 2>&1; then
	print -u2 "error: malformed gitleaks report was accepted"
	exit 1
fi
grep -F -- 'error: gitleaks produced no complete JSON null/array report' "$malformed_out" >/dev/null || {
	print -u2 "error: missing malformed-report diagnostic"
	exit 1
}

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

# FAC-251 budget-derivation scale checks run LAST, after the optional
# real-scanner section above, so real gosec never sees the scaled fixture:
# only the mock scanners scan the synthetic package dirs below.
# Scale-following: 80 additional tracked package directories must raise the
# derived budget to 90 + 3*81 = 333s — above the floor, below the cap.
cat << 'EOF' > "$mock_bin/gosec"
#!/usr/bin/env zsh
for arg in "$@"; do
	if [[ "$arg" == -out=* ]]; then
		print '{"Issues":null}' > "${arg#-out=}"
	fi
done
exit 0
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
for (( pi=1; pi<=80; pi++ )); do
	mkdir -p "scalepkg$pi"
	print "package scalepkg$pi" > "scalepkg$pi/p.go"
done
git add -A
git -c commit.gpgsign=false commit -qm 'test: scale the fixture scan surface to 81 packages'
scale_out="$tmp/gosec-scale.out"
if ! PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$scale_out" 2>&1; then
	print -u2 "error: security-gate failed at 81-package fixture scale"
	exit 1
fi
grep -F -- '==> gosec budget 333s derived from 81 scanned Go packages' "$scale_out" >/dev/null || {
	print -u2 "error: gosec budget did not follow the measured scan surface at 81 packages"
	exit 1
}

# Cap: 1001 packages must cap the derived budget at the strict 900s hang
# bound, and the diagnostic must name the measured surface.
for (( pi=81; pi<=1000; pi++ )); do
	mkdir -p "scalepkg$pi"
	print "package scalepkg$pi" > "scalepkg$pi/p.go"
done
git add -A
git -c commit.gpgsign=false commit -qm 'test: scale the fixture scan surface to 1001 packages'
cap_out="$tmp/gosec-cap.out"
if ! PATH="$mock_bin:$PATH" ./scripts/security-gate.zsh >"$cap_out" 2>&1; then
	print -u2 "error: security-gate failed at 1001-package fixture scale"
	exit 1
fi
grep -F -- '==> gosec budget 900s derived from 1001 scanned Go packages' "$cap_out" >/dev/null || {
	print -u2 "error: gosec budget derivation missing the strict 900s hang cap"
	exit 1
}
