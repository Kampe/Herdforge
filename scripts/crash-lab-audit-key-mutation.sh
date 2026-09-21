#!/usr/bin/env zsh
# Hosted GitHub Linux only. Mutates production guards, requires named test FAIL, restores.
set -euo pipefail

if [[ "${GITHUB_ACTIONS:-}" != "true" || "${RUNNER_OS:-}" != "Linux" ]]; then
  print -u2 "refusing: hosted Linux mutation driver only"
  exit 1
fi

FILE=pkg/signerboundary/key_audit.go
FOCUS='TestVerifyKeyAudit_|TestReadBoundedAuditSeed_|TestMutation_KeyAudit'
restore() { git checkout -- "$FILE"; }
trap restore EXIT

run_focus() {
  go test -count=1 -timeout=60s ./pkg/signerboundary -run "$1"
}

must_fail_named() {
  local mutant=$1
  local test=$2
  local assertion=$3
  local out
  set +e
  out="$(go test -count=1 -timeout=60s ./pkg/signerboundary -run "^${test}$" 2>&1)"
  local rc=$?
  set -e
  print -r -- "$out"
  if (( rc == 0 )); then
    print -u2 "mutant $mutant: $test passed; production guard not proven"
    exit 1
  fi
  print -r -- "$out" | grep -q "FAIL:[[:space:]]*$test" || {
    print -u2 "mutant $mutant: not a named FAIL for $test (compile/setup?)"
    print -u2 "$out"
    exit 1
  }
  print -r -- "$out" | grep -F -q "$assertion" || {
    print -u2 "mutant $mutant: missing assertion text: $assertion"
    exit 1
  }
  restore
}

perl -i -pe 's/if st\.Nlink != 1 \{/if false {/' "$FILE"
must_fail_named nlink TestMutation_KeyAudit_NlinkZeroNeverOK 'nlink=0 must fail even with a matching signature'

perl -i -pe 's/if !ed25519\.Verify\(pub, vr\.Canonical\(\), sig\) \{/if false {/' "$FILE"
must_fail_named signature TestVerifyKeyAudit_RejectsAlteredSignedClaims 'altered signed claims must fail signature'

perl -i -pe 's/if st\.Identity != expectedIdentity \{/if false {/' "$FILE"
must_fail_named identity TestVerifyKeyAudit_RejectsWrongNonceIdentityPathUIDPID 'want identity mismatch'

perl -i -pe 's/if len\(data\) > maxAuditSeedRead \{/if false {/' "$FILE"
must_fail_named overflow TestReadBoundedAuditSeed_RejectsOverflow 'want overflow'

perl -i -pe 's/if !ed25519\.Verify\(pub, vr\.Canonical\(\), sig\) \{/if !ed25519.Verify(pub, vr.Canonical(), sig) \&\& len(pub) == ed25519.PublicKeySize {/' "$FILE"
must_fail_named emptypub TestVerifyKeyAudit_RejectsMissingPinnedKey 'empty published key must fail'

restore
run_focus "$FOCUS"
print -r -- "audit-key mutation driver ok"
