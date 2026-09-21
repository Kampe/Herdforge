#!/usr/bin/env zsh
# Hosted GitHub Linux only. Sequential mutants of production guards.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi

FILE=pkg/signerboundary/key_audit.go
FOCUS='TestVerifyKeyAudit_|TestReadBoundedAuditSeed_|TestMutation_KeyAudit'
LOG="${MUTATION_LOG:-audit-key-mutation.log}"
: >"$LOG"
log() { print -r -- "$*" | tee -a "$LOG"; }

restore() { git checkout -- "$FILE"; }
trap restore EXIT

if ! git diff --quiet -- "$FILE" || ! git diff --cached --quiet -- "$FILE"; then
  print -u2 "refuse dirty $FILE before mutants"
  exit 1
fi

count_lit() {
  grep -F -c -- "$1" "$FILE"
}

replace_once() {
  local from=$1
  local to=$2
  local n
  n="$(count_lit "$from")"
  if [[ "$n" != "1" ]]; then
    print -u2 "literal not unique (count=$n): $from"
    exit 1
  fi
  python3 - "$FILE" "$from" "$to" <<'PY'
import pathlib, sys
path, src, dst = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
text = path.read_text()
if text.count(src) != 1:
    raise SystemExit(f"count {text.count(src)} for {src!r}")
path.write_text(text.replace(src, dst, 1))
PY
}

must_fail_named() {
  local mutant=$1
  local test=$2
  local assertion=$3
  log "MUTANT $mutant expect FAIL $test assertion=$assertion"
  set +e
  local out
  out="$(go test -count=1 -timeout=60s ./pkg/signerboundary -run "^${test}$" 2>&1)"
  local rc=$?
  set -e
  print -r -- "$out" | tee -a "$LOG"
  if (( rc == 0 )); then
    print -u2 "mutant $mutant: $test passed; guard not proven"
    exit 1
  fi
  print -r -- "$out" | grep -E -q "FAIL:[[:space:]]*${test}( |$)" || {
    print -u2 "mutant $mutant: not named FAIL for $test"
    exit 1
  }
  print -r -- "$out" | grep -F -q "$assertion" || {
    print -u2 "mutant $mutant: missing assertion: $assertion"
    exit 1
  }
  restore
  if ! git diff --quiet -- "$FILE"; then
    print -u2 "restore left dirty $FILE"
    exit 1
  fi
}

log "BASELINE focused PASS"
go test -count=1 -timeout=60s ./pkg/signerboundary -run "$FOCUS" | tee -a "$LOG"

# nonce check is first field match after domain. Skip it: /nonce expected-nonce
# differs, later identity/path/uid/pid/signature still match original -> err nil.
replace_once 'if st.Nonce != req.Nonce || strings.TrimSpace(st.Nonce) == "" {' 'if false {'
must_fail_named nonce TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/nonce 'want nonce mismatch'

# identity skip: expected wrong-id, path/uid/pid/sig still original -> err nil.
replace_once 'if st.Identity != expectedIdentity {' 'if false {'
must_fail_named identity TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/identity 'want identity mismatch'

# path skip: expected other path, identity/uid/pid/sig original -> err nil.
replace_once 'if st.Path != expectedPath {' 'if false {'
must_fail_named path TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/path 'want path mismatch'

# uid skip: expected 8 vs claim 9; pid/mode/nlink/sig original -> err nil.
replace_once 'if st.OwnerUID != expectedUID || st.ServerUID != expectedUID {' 'if false {'
must_fail_named uid TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/uid 'want uid mismatch'

# pid skip: expected 7 vs 42; remaining static+sig original -> err nil.
replace_once 'if expectedPID > 0 && st.ServerPID != expectedPID {' 'if false {'
must_fail_named pid TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/pid 'want pid mismatch'

# nlink skip: fixture re-signs nlink=0 so sig matches; only nlink guard remains.
replace_once 'if st.Nlink != 1 {' 'if false {'
must_fail_named nlink TestMutation_KeyAudit_NlinkZeroNeverOK 'nlink=0 must fail even with a matching signature'

# verify skip: OwnerUID+ServerUID+expected all 99, original sig; static pass, sig skipped -> err nil.
replace_once 'if !ed25519.Verify(pub, vr.Canonical(), sig) {' 'if false {'
must_fail_named signature TestVerifyKeyAudit_RejectsAlteredSignedClaims 'altered signed claims must fail signature'

# overflow skip: 73-byte read returns without error -> want overflow.
replace_once 'if len(data) > maxAuditSeedRead {' 'if false {'
must_fail_named overflow TestReadBoundedAuditSeed_RejectsOverflow 'want overflow'

# empty-pub inject at verifyKeyAudit entry; size/verify never reached for nil pub.
replace_once 'func verifyKeyAudit(pub ed25519.PublicKey, expectedPath, expectedIdentity string, expectedUID, expectedPID int, req SignRequest, st KeyAuditStatement, sig []byte) error {' $'func verifyKeyAudit(pub ed25519.PublicKey, expectedPath, expectedIdentity string, expectedUID, expectedPID int, req SignRequest, st KeyAuditStatement, sig []byte) error {\n\tif len(pub) == 0 { return nil }'
must_fail_named emptypub TestVerifyKeyAudit_RejectsMissingPinnedKey/nil 'nil published key must fail'

restore
if ! git diff --quiet -- "$FILE"; then
  print -u2 "final restore left dirty $FILE"
  exit 1
fi
log "POST-MUTANT focused PASS"
go test -count=1 -timeout=60s ./pkg/signerboundary -run "$FOCUS" | tee -a "$LOG"
log "audit-key mutation driver ok"
