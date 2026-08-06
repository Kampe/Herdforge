package scopefence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ReleaseProof is a deterministic test/deduplication identifier only. It is
// never a production release credential; production uses ReleaseAuthority.
// It carries no secret and is intended for a separately authenticated
// verifier to compare byte-for-byte.
func ReleaseProof(req ReleaseRequest) string {
	canonical, _ := canonicalScope(req.Scope)
	value := struct {
		Identity
		Generation    int64
		Scope         Scope
		State         State
		GraphRevision string
		GraphFiles    int
		Authority     Authority
	}{req.Identity, req.Generation, canonical, req.State, req.GraphRevision, req.GraphFiles, req.Authority}
	encoded, _ := json.Marshal(value)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
