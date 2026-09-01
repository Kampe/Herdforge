package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/daemon"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/verifier"
)

const configuredVerificationFixture = `version: "1"
project:
  name: herdforge-test
task_provider:
  type: memory
verification:
  test_command: "go test ./..."
  test_timeout: "30m"
`

func TestVerificationCommandProfileAppliesConfiguredTestTimeout(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `version: "1"
project:
  name: timeout-fixture
task_provider:
  type: memory
verification:
  test_command: "go test ./..."
  test_timeout: "45m"
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	profile, revision, err := verificationCommandProfile(root)
	if err != nil {
		t.Fatal(err)
	}
	if profile.TestCommand != "go test ./..." || profile.TestTimeout != 45*time.Minute {
		t.Fatalf("profile test command/timeout = %q/%s, want configured values", profile.TestCommand, profile.TestTimeout)
	}
	if revision == "" || revision == "default" {
		t.Fatalf("configured profile revision = %q, want content revision", revision)
	}
}

func TestVerificationCommandProfileSkipsGoBuildForNonGoRepository(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := `version: "1"
project:
  name: node-fixture
task_provider:
  type: memory
verification:
  test_command: "bin/ci-local"
`
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, _, err := verificationCommandProfile(root)
	if err != nil {
		t.Fatal(err)
	}
	if profile.BuildCommand != "true" {
		t.Fatalf("non-Go profile build command = %q, want true", profile.BuildCommand)
	}
}

func TestConfiguredTimeoutVerifyReceiptAdmitsWithLiveBinding(t *testing.T) {
	root, keyDir := newConfiguredVerificationFixture(t)
	binary := buildHerd(t)

	cmd := exec.Command(binary, "verify", "--build", "go build ./...", "--test", "go test ./...", ".")
	cmd.Dir = root
	cmd.Env = append(reviewTestEnv(), dispatch.KeyDirEnv+"="+keyDir, "HERD_BIN=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("configured verify failed: %v\n%s", err, out)
	}

	receipt := exactTestReceipt(t, root, []string{"go", "test", "-timeout=30m0s", "./..."})
	machine, err := lifecycle.NewMachine(filepath.Join(root, defaultLifecycleDB))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	if err := daemon.SeedLifecycleToBuilding(machine, "FAC-1", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind, err := bindingForWorktreeAtRoot(nil, machine, "FAC-1", root, root)
	if err != nil {
		t.Fatal(err)
	}
	gate, err := daemon.NewCompletionGate(verifier.NewVerifier("go test ./..."), filepath.Join(root, defaultReceiptDir), machine)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.AdmitReview(context.Background(), bind, receipt.Digest); err != nil {
		t.Fatalf("fresh exact full-test PASS receipt was not admitted: %v", err)
	}

	for _, changed := range []struct {
		name, config string
	}{
		{name: "timeout", config: strings.Replace(configuredVerificationFixture, `test_timeout: "30m"`, `test_timeout: "31m"`, 1)},
		{name: "command", config: strings.Replace(configuredVerificationFixture, `go test ./...`, `go test ./pkg/...`, 1)},
	} {
		t.Run(changed.name+" change invalidates", func(t *testing.T) {
			writeVerificationConfig(t, root, changed.config)
			changedBind, err := bindingForWorktreeAtRoot(nil, machine, "FAC-1", root, root)
			if err != nil {
				t.Fatal(err)
			}
			if changedBind.ProfileDigest == bind.ProfileDigest {
				t.Fatal("configured execution profile change did not change its digest")
			}
			if _, err := gate.AdmitReview(context.Background(), changedBind, receipt.Digest); !errors.Is(err, daemon.ErrBindingMismatch) {
				t.Fatalf("changed profile must fail binding admission, got %v", err)
			}
		})
	}

	writeVerificationConfig(t, root, configuredVerificationFixture)
	expanded := exec.Command(binary, "verify", "--build", "go build ./...", "--test", "go test -timeout=30m0s ./...", ".")
	expanded.Dir = root
	expanded.Env = cmd.Env
	out, err := expanded.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "commands must match repository profile") {
		t.Fatalf("expanded command bypassed raw configured-command validation: err=%v\n%s", err, out)
	}
}

func TestVerificationBindingRejectsMalformedConfiguredProfile(t *testing.T) {
	for _, tt := range []struct {
		name, config string
	}{
		{name: "timeout", config: strings.Replace(configuredVerificationFixture, `test_timeout: "30m"`, `test_timeout: "invalid"`, 1)},
		{name: "command", config: strings.Replace(configuredVerificationFixture, `go test ./...`, `go test 'unterminated`, 1)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root, _ := newConfiguredVerificationFixture(t)
			writeVerificationConfig(t, root, tt.config)
			machine, err := lifecycle.NewMachine(filepath.Join(root, defaultLifecycleDB))
			if err != nil {
				t.Fatal(err)
			}
			defer machine.Close()
			if err := daemon.SeedLifecycleToBuilding(machine, "FAC-1", "herdforge", 1); err != nil {
				t.Fatal(err)
			}
			if _, err := bindingForWorktreeAtRoot(nil, machine, "FAC-1", root, root); err == nil {
				t.Fatal("malformed configured execution profile must fail closed")
			}
		})
	}
}

func TestVerificationBindingLeavesDefaultLocalProfileUnchanged(t *testing.T) {
	root := t.TempDir()
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	gitIn(t, root, "commit", "--allow-empty", "-m", "feat: local candidate")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile, revision, err := verificationCommandProfile(root)
	if err != nil {
		t.Fatal(err)
	}
	if revision != "default" {
		t.Fatalf("revision = %q, want default", revision)
	}
	machine, err := lifecycle.NewMachine(filepath.Join(root, defaultLifecycleDB))
	if err != nil {
		t.Fatal(err)
	}
	defer machine.Close()
	if err := daemon.SeedLifecycleToBuilding(machine, "FAC-1", "herdforge", 1); err != nil {
		t.Fatal(err)
	}
	bind, err := bindingForWorktreeAtRoot(nil, machine, "FAC-1", root, root)
	if err != nil {
		t.Fatal(err)
	}
	if bind.ProfileDigest != profile.Digest() || profile.TestCommand != "go test ./..." {
		t.Fatalf("default local profile changed: bind=%q profile=%+v", bind.ProfileDigest, profile)
	}
}

func newConfiguredVerificationFixture(t *testing.T) (string, string) {
	t.Helper()
	root, keyDir := t.TempDir(), t.TempDir()
	gitIn(t, root, "init", "-b", "main")
	gitIn(t, root, "config", "user.email", "test@example.invalid")
	gitIn(t, root, "config", "user.name", "test")
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeVerificationConfig(t, root, configuredVerificationFixture)
	for name, data := range map[string]string{
		".gitignore": ".herd/\nTASK-CONTEXT.json\n",
		"go.mod":     "module example.invalid/verificationfixture\n\ngo 1.23\n",
		"fixture.go": "package verificationfixture\n",
	} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, root, "add", ".gitignore", "go.mod", "fixture.go")
	gitIn(t, root, "commit", "-m", "chore: fixture base")
	gitIn(t, root, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, root, "add", "candidate.txt")
	gitIn(t, root, "commit", "-m", "fix: candidate")
	attestKeyDir(t, keyDir)
	writeSignedReceipt(t, keyDir, root, root, nil)
	return root, keyDir
}

func writeVerificationConfig(t *testing.T, root, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func exactTestReceipt(t *testing.T, root string, command []string) verifier.Receipt {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, defaultReceiptDir))
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(command, "\x00")
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, defaultReceiptDir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var receipt verifier.Receipt
		if err := json.Unmarshal(data, &receipt); err != nil {
			t.Fatal(err)
		}
		if receipt.Outcome == verifier.OutcomePASS && strings.Join(receipt.Command, "\x00") == want {
			return receipt
		}
	}
	t.Fatalf("no exact PASS receipt for %q", strings.Join(command, " "))
	return verifier.Receipt{}
}
