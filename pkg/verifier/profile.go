package verifier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
)

// CommandProfile is the immutable command set authorized by repository
// configuration. Its digest is persisted in every managed verification
// receipt, so a caller cannot replace a real test with a vacuous command.
type CommandProfile struct {
	ID               string        `json:"id"`
	BuildCommand     string        `json:"build_command"`
	TestCommand      string        `json:"test_command"`
	TestTimeout      time.Duration `json:"test_timeout_ns,omitempty"`
	PreflightCommand string        `json:"preflight_command,omitempty"`
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

// ApplyTestTimeout makes the Go test timeout explicit. The go command's
// default is ten minutes, which is too short for the full suite under normal
// concurrent fleet load. Other test runners own their timeout semantics and
// are left unchanged. An existing Go timeout remains authoritative.
func ApplyTestTimeout(command string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return command, nil
	}
	argv, err := parseArgv(command)
	if err != nil {
		return "", fmt.Errorf("parse test command: %w", err)
	}
	if len(argv) < 2 || filepath.Base(argv[0]) != "go" || argv[1] != "test" {
		return command, nil
	}
	for _, arg := range argv[2:] {
		if arg == "-timeout" || strings.HasPrefix(arg, "-timeout=") {
			return command, nil
		}
	}
	argv = append(argv, "")
	copy(argv[3:], argv[2:])
	argv[2] = "-timeout=" + timeout.String()
	return joinArgv(argv), nil
}

func joinArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		if arg != "" && strings.IndexFunc(arg, func(r rune) bool {
			return !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '=' ||
				(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9'))
		}) < 0 {
			parts[i] = arg
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
	}
	return strings.Join(parts, " ")
}

// ProfileIDConfigured is the stable identity of the repository-configured
// verification profile, persisted in managed receipts and bindings.
const ProfileIDConfigured = "config-verification"

// ResolvedProfile is the immutable command set a repository authorizes for
// managed verification, derived from .herd/herd.yaml exactly as the managed
// gate binds it: the configured base profile plus the executed form with the
// test timeout applied. Refusal explains why the repository profile could not
// be resolved; when non-empty, no receipt may be admitted.
type ResolvedProfile struct {
	Base      CommandProfile
	Execution CommandProfile
	// Name is the binding identity receipts carry: the profile ID plus
	// "+preflight" when a preflight command is configured.
	Name string
	// Revision binds the profile to the exact herd.yaml bytes it came from.
	Revision string
	Refusal  string
}

// CommandAdmitted reports whether an argv is one of the repository's
// authorized full-suite test commands (the configured form or the executed
// form with the test timeout applied).
func (p ResolvedProfile) CommandAdmitted(command []string) bool {
	joined := strings.Join(command, " ")
	if joined == "" {
		return false
	}
	return joined == strings.TrimSpace(p.Base.TestCommand) || joined == strings.TrimSpace(p.Execution.TestCommand)
}

// ReceiptAdmitted reports whether the profile identity a receipt carries
// (when present) matches this repository's live derivation, mirroring the
// managed completion gate's bindMatchesReceipt binding.
func (p ResolvedProfile) ReceiptAdmitted(receipt Receipt) bool {
	if receipt.VerificationProfile != "" && receipt.VerificationProfile != p.Name {
		return false
	}
	if receipt.ProfileDigest != "" && receipt.ProfileDigest != p.Base.Digest() && receipt.ProfileDigest != p.Execution.Digest() {
		return false
	}
	if receipt.ConfigRevision != "" && receipt.ConfigRevision != p.Revision {
		return false
	}
	return true
}

// ResolveProfile derives the verification command profile for a repository
// root: the configured test command from .herd/herd.yaml (required when the
// file exists), the default go-test profile otherwise, and the executed form
// with the test timeout applied for a configured repository. This is the one
// derivation both the managed completion gate and the candidate index admit
// receipts against.
func ResolveProfile(root string) ResolvedProfile {
	buildCommand := "true"
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
		// Repositories without a Go module must not be forced through a
		// meaningless Go build. Their declared test command (for example
		// bin/ci-local) owns build/typecheck coverage; the no-op build keeps
		// the receipt profile explicit without claiming a Go build ran.
		buildCommand = "go build ./..."
	}
	base := CommandProfile{
		ID:           ProfileIDConfigured,
		BuildCommand: buildCommand,
		TestCommand:  "go test ./...",
		TestTimeout:  30 * time.Minute,
	}
	revision := "default"
	data, err := os.ReadFile(filepath.Join(root, ".herd", "herd.yaml"))
	if err == nil {
		cfg, cfgErr := config.ParseConfig(data)
		if cfgErr != nil {
			return ResolvedProfile{Refusal: fmt.Sprintf("parse verification config: %v", cfgErr)}
		}
		if strings.TrimSpace(cfg.Verification.TestCommand) == "" {
			return ResolvedProfile{Refusal: "verification.test_command is required"}
		}
		base.TestCommand = strings.TrimSpace(cfg.Verification.TestCommand)
		if raw := strings.TrimSpace(cfg.Verification.TestTimeout); raw != "" {
			timeout, parseErr := time.ParseDuration(raw)
			if parseErr != nil || timeout <= 0 {
				return ResolvedProfile{Refusal: fmt.Sprintf("verification.test_timeout must be a positive Go duration: %q", raw)}
			}
			base.TestTimeout = timeout
		}
		base.PreflightCommand = strings.TrimSpace(cfg.Verification.PreflightCommand)
		sum := sha256.Sum256(data)
		revision = "sha256:" + hex.EncodeToString(sum[:])
	} else if !errors.Is(err, os.ErrNotExist) {
		return ResolvedProfile{Refusal: fmt.Sprintf("read verification config: %v", err)}
	}
	execution := base
	if revision != "default" {
		testCommand, timeoutErr := ApplyTestTimeout(base.TestCommand, base.TestTimeout)
		if timeoutErr != nil {
			return ResolvedProfile{Refusal: fmt.Sprintf("apply test timeout: %v", timeoutErr)}
		}
		execution.TestCommand = testCommand
	}
	name := base.ID
	if base.PreflightCommand != "" {
		name += "+preflight"
	}
	return ResolvedProfile{Base: base, Execution: execution, Name: name, Revision: revision}
}
