package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
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

func TestBuildNativeRuntimeKillsProcessGroupOnCancel(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0755); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\necho $$ > pgid.txt\n/bin/sleep 120 &\necho $! > child.txt\n/bin/sleep 120\n"
	if err := os.WriteFile(filepath.Join(dir, "scripts", "build-herd.sh"), []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	err := buildNativeRuntime(ctx, dir)
	if err == nil || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("timed-out build must report the deadline: %v", err)
	}
	childBody, err := os.ReadFile(filepath.Join(dir, "child.txt"))
	if err != nil {
		t.Skipf("script never reached the descendant spawn: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(childBody)))
	if err != nil {
		t.Fatal(err)
	}
	// The descendant must be reaped with its process group; poll briefly.
	alive := false
	for i := 0; i < 40; i++ {
		if err := syscall.Kill(pid, 0); err == nil {
			alive = true
			time.Sleep(50 * time.Millisecond)
			continue
		}
		alive = false
		break
	}
	if alive {
		// Own spawned process: terminate it before failing so the fixture
		// never leaks a sleeper.
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("build descendant %d survived the process-group teardown", pid)
	}
}
