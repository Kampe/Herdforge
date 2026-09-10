package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseNativeInstallRequiresExplicitIdentity(t *testing.T) {
	if _, err := parseNativeInstallArgs([]string{"--source", "src", "--revision", strings.Repeat("a", 40)}); err == nil {
		t.Fatal("missing explicit target must be rejected")
	}
	opts, err := parseNativeInstallArgs([]string{
		"--source", "src", "--revision", strings.Repeat("a", 40), "--target", "root",
	})
	if err != nil {
		t.Fatal(err)
	}
	if opts.act {
		t.Fatal("install must default to dry-run")
	}
	if opts, err = parseNativeInstallArgs([]string{
		"--source", "src", "--revision", strings.Repeat("a", 40), "--target", "root", "--act",
	}); err != nil || !opts.act {
		t.Fatalf("--act parse: %+v %v", opts, err)
	}
}

func TestNativeInstallRefusesForeignTargetBeforeBuild(t *testing.T) {
	root := t.TempDir()
	runNativeInstallGit(t, root, "init", "-q", "-b", "main")
	runNativeInstallGit(t, root, "config", "user.email", "test@example.com")
	runNativeInstallGit(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte("bin/herd\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("fixture\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runNativeInstallGit(t, root, "add", ".")
	runNativeInstallGit(t, root, "commit", "-q", "-m", "fixture")
	revision := runNativeInstallGit(t, root, "rev-parse", "HEAD")
	runNativeInstallGit(t, root, "update-ref", "refs/remotes/origin/main", revision)
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "herd"), []byte("foreign\n"), 0755); err != nil {
		t.Fatal(err)
	}

	built := false
	_, err := executeNativeInstall(context.Background(), nativeInstallOptions{
		source: root, revision: revision, target: root, act: true,
	}, func(context.Context, string) error {
		built = true
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "identity is unknown") {
		t.Fatalf("foreign target was not refused: %v", err)
	}
	if built {
		t.Fatal("foreign target refusal must precede source build")
	}
}

func runNativeInstallGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
