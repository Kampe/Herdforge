package verifier

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"
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
