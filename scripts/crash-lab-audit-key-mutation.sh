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

if ! git diff --quiet -- "$FILE" || ! git diff --cached --quiet -- "$FILE"; then
  print -u2 "refuse dirty $FILE before mutants"
  exit 1
fi
trap restore EXIT

replace_once() {
  local from=$1
  local to=$2
  local n
  n="$(grep -F -c -- "$from" "$FILE" || true)"
  if [[ "$n" != "1" ]]; then
    print -u2 "literal not unique (count=$n): $from"
    exit 1
  fi
  FROM="$from" TO="$to" perl -i -pe 'BEGIN { $from = $ENV{FROM}; $to = $ENV{TO} } s/\Q$from\E/$to/' "$FILE"
  if [[ "$to" != *$'\n'* ]]; then
    n="$(grep -F -c -- "$to" "$FILE" || true)"
    if [[ "$n" != "1" ]]; then
      print -u2 "replacement not unique after edit (count=$n): $to"
      exit 1
    fi
  fi
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

# Re-signed request with new nonce, statement.Nonce original: only nonce guard rejects.
replace_once 'if st.Nonce != req.Nonce || strings.TrimSpace(st.Nonce) == "" {' 'if false {'
must_fail_named nonce TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/nonce 'want nonce mismatch'

replace_once 'if st.Identity != expectedIdentity {' 'if false {'
must_fail_named identity TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/identity 'want identity mismatch'

replace_once 'if st.Path != expectedPath {' 'if false {'
must_fail_named path TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/path 'want path mismatch'

replace_once 'if st.OwnerUID != expectedUID || st.ServerUID != expectedUID {' 'if false {'
must_fail_named uid TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/uid 'want uid mismatch'

replace_once 'if expectedPID > 0 && st.ServerPID != expectedPID {' 'if false {'
must_fail_named pid TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID/pid 'want pid mismatch'

replace_once 'if st.Nlink != 1 {' 'if false {'
must_fail_named nlink TestMutation_KeyAudit_NlinkZeroNeverOK 'nlink=0 must fail even with a matching signature'

# Static UID fields all 99; original signature; skip Verify -> err nil.
replace_once 'if !ed25519.Verify(pub, vr.Canonical(), sig) {' 'if false {'
must_fail_named signature TestVerifyKeyAudit_RejectsAlteredSignedClaims 'altered signed claims must fail signature'

replace_once 'if len(data) > maxAuditSeedRead {' 'if false {'
must_fail_named overflow TestReadBoundedAuditSeed_RejectsOverflow 'want overflow'

SIG='func verifyKeyAudit(pub ed25519.PublicKey, expectedPath, expectedIdentity string, expectedUID, expectedPID int, req SignRequest, st KeyAuditStatement, sig []byte) error {'
replace_once "$SIG" "$SIG"$'\n\tif len(pub) == 0 { return nil }'
must_fail_named emptypub TestVerifyKeyAudit_RejectsMissingPinnedKey/nil 'nil published key must fail'

restore
if ! git diff --quiet -- "$FILE"; then
  print -u2 "final restore left dirty $FILE"
  exit 1
fi
log "POST-MUTANT focused PASS"
go test -count=1 -timeout=60s ./pkg/signerboundary -run "$FOCUS" | tee -a "$LOG"
log "audit-key mutation driver ok"
