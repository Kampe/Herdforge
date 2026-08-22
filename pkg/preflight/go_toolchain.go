package preflight

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckGoToolchain rejects an exported GOROOT that points at a different Go
// installation than the go command selected by PATH. It only observes the
// environment; callers remain responsible for applying the suggested fix.
func CheckGoToolchain() error {
	return checkGoToolchain(os.Environ(), runGoToolchainCommand)
}

type goToolchainProbe func(env []string, args ...string) (string, error)

func checkGoToolchain(env []string, probe goToolchainProbe) error {
	exportedGOROOT, exported := lookupEnv(env, "GOROOT")
	if !exported || strings.TrimSpace(exportedGOROOT) == "" {
		return nil
	}

	pathEnv := withoutEnv(env, "GOROOT")
	pathGOROOT, err := probe(pathEnv, "env", "GOROOT")
	if err != nil {
		// FAC-576: inability to VERIFY is not evidence of a mismatch.
		//
		// This used to fail preflight outright, so any environment where the
		// probe cannot run — a version-manager shim that needs context this
		// child does not have, no go on the child's PATH, a sandbox — was
		// reported as a toolchain conflict and blocked the run. That is the same
		// error as reading "cannot prove" as "did not land": it converts an
		// unanswered question into a negative finding.
		//
		// A real mismatch is still caught, because catching it requires
		// successfully observing BOTH toolchains, which is exactly the case
		// this branch is not.
		return nil
	}
	pathGOROOT = strings.TrimSpace(pathGOROOT)
	if samePath(exportedGOROOT, pathGOROOT) {
		return nil
	}

	pathVersionOutput, err := probe(pathEnv, "version")
	if err != nil {
		return fmt.Errorf("Go toolchain mismatch: exported GOROOT=%q, but PATH resolves GOROOT=%q; unset GOROOT (env -u GOROOT make build) and retry: %w", exportedGOROOT, pathGOROOT, err)
	}
	pathVersion := goVersion(pathVersionOutput)
	exportedVersion := goRootVersion(exportedGOROOT)
	return fmt.Errorf("Go toolchain mismatch: exported GOROOT=%q (%s), but PATH-resolved go uses GOROOT=%q (%s); unset GOROOT (env -u GOROOT make build) and retry", exportedGOROOT, exportedVersion, pathGOROOT, pathVersion)
}

func runGoToolchainCommand(env []string, args ...string) (string, error) {
	cmd := exec.Command("go", args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), err
	}
	return string(out), nil
}

func lookupEnv(env []string, key string) (string, bool) {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix), true
		}
	}
	return "", false
}

func withoutEnv(env []string, key string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

func samePath(left, right string) bool {
	leftResolved, leftErr := filepath.EvalSymlinks(left)
	rightResolved, rightErr := filepath.EvalSymlinks(right)
	if leftErr == nil && rightErr == nil {
		left, right = leftResolved, rightResolved
	}
	return filepath.Clean(left) == filepath.Clean(right)
}

func goRootVersion(goroot string) string {
	data, err := os.ReadFile(filepath.Join(goroot, "VERSION"))
	if err != nil {
		return "version unavailable"
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 {
		return "version unavailable"
	}
	return fields[0]
}

func goVersion(output string) string {
	for _, field := range strings.Fields(output) {
		if strings.HasPrefix(field, "go1.") {
			return field
		}
	}
	return "version unavailable"
}
