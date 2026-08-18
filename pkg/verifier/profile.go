package verifier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// CommandProfile is the immutable command set authorized by repository
// configuration. Its digest is persisted in every managed verification
// receipt, so a caller cannot replace a real test with a vacuous command.
type CommandProfile struct {
	ID               string `json:"id"`
	BuildCommand     string `json:"build_command"`
	TestCommand      string `json:"test_command"`
	PreflightCommand string `json:"preflight_command,omitempty"`
}

func (p CommandProfile) Digest() string {
	data, _ := json.Marshal(p)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (p CommandProfile) Matches(build, test, preflight string) bool {
	return strings.TrimSpace(build) == strings.TrimSpace(p.BuildCommand) &&
		strings.TrimSpace(test) == strings.TrimSpace(p.TestCommand) &&
		strings.TrimSpace(preflight) == strings.TrimSpace(p.PreflightCommand)
}
