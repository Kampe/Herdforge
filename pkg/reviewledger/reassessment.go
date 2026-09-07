package reviewledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// VerdictEventDigest names the exact prior immutable verdict, not just its SHA.
func VerdictEventDigest(row LedgerRow) string {
	b, _ := json.Marshal(row)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CheckReassessment is shared by dry-run and the locked append path.
// It returns true only for a byte-identical replay of a prior reassessment.
func CheckReassessment(prior LedgerRow, opts VerdictOpts) (bool, error) {
	if opts.Reassesses == "" {
		return false, fmt.Errorf("reassessment must bind the prior verdict event")
	}
	if prior.SHA != opts.SHA || prior.Reviewer != opts.Reviewer || prior.Task != opts.Task || prior.ReviewerFamily != opts.ReviewerFamily || prior.BuilderFamily != opts.BuilderFamily {
		return false, fmt.Errorf("reassessment identity differs from prior verdict")
	}
	if prior.Reassesses == opts.Reassesses && prior.ArtifactDigest == opts.ArtifactDigest && prior.VerificationDigest == opts.VfyDigest && prior.Verdict == string(opts.Verdict) {
		return true, nil
	}
	if VerdictEventDigest(prior) != opts.Reassesses {
		return false, fmt.Errorf("stale or unbound reassessment")
	}
	if !FamilyAllowlist[opts.ReviewerFamily] || !FamilyAllowlist[opts.BuilderFamily] || opts.ReviewerFamily == opts.BuilderFamily {
		return false, fmt.Errorf("reassessment requires independent authenticated families")
	}
	proof, err := hex.DecodeString(opts.ArtifactDigest)
	if err != nil || len(proof) != 32 || opts.VfyDigest == "" || opts.VfyDigest == prior.VerificationDigest || opts.ArtifactDigest == prior.ArtifactDigest {
		return false, fmt.Errorf("reassessment requires new artifact and verification evidence")
	}
	if opts.RetryOf != "" {
		return false, fmt.Errorf("reassessment cannot supersede another reviewer")
	}
	return false, nil
}
