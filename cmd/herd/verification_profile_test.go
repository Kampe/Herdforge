package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
