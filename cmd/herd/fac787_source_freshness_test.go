package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-787: a factory executable copied into an unrelated non-Go application
// must not compare the application's HEAD with the factory build revision.
func TestVerifyCLI_UnrelatedApplicationDoesNotBorrowFactoryFreshness(t *testing.T) {
	factoryBinary := buildHerd(t)
	app := t.TempDir()
	gitIn(t, app, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(app, "README.md"), []byte("unrelated application\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, app, "add", "README.md")
	gitIn(t, app, "commit", "-q", "-m", "application")
	gitIn(t, app, "update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(app, "README.md"), []byte("unrelated application change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, app, "commit", "-q", "-am", "application work")
	if err := os.MkdirAll(filepath.Join(app, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(app, "bin", "herd")
	binary, err := os.ReadFile(factoryBinary)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(installed, binary, 0o755); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(installed, "verify", "--build", "true", "--test", "true", ".")
	cmd.Dir = app
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("unrelated application was rejected: %v\n%s", err, out)
	}
}

func TestVerifyCLI_GenuineFactorySourceRejectsStaleBinary(t *testing.T) {
	factoryBinary := buildHerd(t)
	source := t.TempDir()
	gitIn(t, source, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(source, "go.mod"), []byte("module github.com/Kampe/Herdforge\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("factory source\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitIn(t, source, "add", "go.mod", "README.md")
	gitIn(t, source, "commit", "-q", "-m", "factory-source")
	if err := os.MkdirAll(filepath.Join(source, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(factoryBinary)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(source, "herd"), filepath.Join(source, "bin", "herd")} {
		if err := os.WriteFile(path, binary, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command(filepath.Join(source, "bin", "herd"), "verify", "--build", "true", "--test", "true", ".")
	cmd.Dir = source
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("stale genuine factory source was accepted:\n%s", out)
	}
	if !strings.Contains(string(out), "stale herd binary") {
		t.Fatalf("missing stale-source refusal:\n%s", out)
	}
}
