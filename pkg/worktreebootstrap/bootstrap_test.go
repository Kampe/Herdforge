package worktreebootstrap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
)

type fakeResolver struct{ identity string }

func (r fakeResolver) Resolve(context.Context, string) (string, error) { return r.identity, nil }

type recordingRunner struct {
	calls int
	env   []string
	err   error
}

func (r *recordingRunner) Run(_ context.Context, _ string, _ []string, env []string) error {
	r.calls++
	r.env = append([]string(nil), env...)
	return r.err
}

func testContract() config.WorktreeBootstrap {
	return config.WorktreeBootstrap{Version: "v1", Toolchain: "go", Command: []string{"go", "mod", "download"}}
}

func TestExecuteWritesDurableReceiptAndReusesExactIdentity(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{}
	exec := Executor{Resolver: fakeResolver{identity: "go1.26.2"}, Runner: runner}
	first, err := exec.Execute(context.Background(), root, testContract())
	if err != nil {
		t.Fatal(err)
	}
	if first.Reused || runner.calls != 1 {
		t.Fatalf("first bootstrap reused=%v calls=%d, want false/1", first.Reused, runner.calls)
	}
	if filepath.IsAbs(first.Receipt.CacheDir) || filepath.IsAbs(first.Receipt.RuntimeDir) {
		t.Fatalf("receipt leaked absolute artifact paths: %+v", first.Receipt)
	}
	for key, rel := range map[string]string{"HERD_BOOTSTRAP_CACHE": first.Receipt.CacheDir, "HERD_BOOTSTRAP_RUNTIME": first.Receipt.RuntimeDir} {
		if !hasEnvWorktreeArtifact(runner.env, key, rel) {
			t.Fatalf("bootstrap environment missing worktree-relative artifact %q", key)
		}
	}
	second, err := exec.Execute(context.Background(), root, testContract())
	if err != nil {
		t.Fatal(err)
	}
	if !second.Reused || runner.calls != 1 {
		t.Fatalf("exact retry reused=%v calls=%d, want true/1", second.Reused, runner.calls)
	}
}

func TestExecuteRepairsMismatchedToolchainArtifacts(t *testing.T) {
	root := t.TempDir()
	runner := &recordingRunner{}
	exec := Executor{Resolver: fakeResolver{identity: "go1"}, Runner: runner}
	first, err := exec.Execute(context.Background(), root, testContract())
	if err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(root, first.Receipt.CacheDir, "stale")
	if err := os.WriteFile(stale, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	exec.Resolver = fakeResolver{identity: "go2"}
	second, err := exec.Execute(context.Background(), root, testContract())
	if err != nil {
		t.Fatal(err)
	}
	if second.Reused || runner.calls != 2 {
		t.Fatalf("mismatch reused=%v calls=%d, want false/2", second.Reused, runner.calls)
	}
	if _, err := os.Stat(stale); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mismatched cache was not repaired: stat=%v", err)
	}
}

func TestExecuteDoesNotWriteReceiptWhenBootstrapFails(t *testing.T) {
	root := t.TempDir()
	exec := Executor{Resolver: fakeResolver{identity: "go1"}, Runner: &recordingRunner{err: errors.New("network unavailable")}}
	_, err := exec.Execute(context.Background(), root, testContract())
	if err == nil || !strings.Contains(err.Error(), "network unavailable") {
		t.Fatalf("bootstrap error=%v, want attributable command failure", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".herd", "bootstrap", "receipt.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed bootstrap wrote a success receipt: %v", err)
	}
}

func TestExecuteRejectsSymlinkedBootstrapArtifactParent(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, ".herd")); err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	exec := Executor{Resolver: fakeResolver{identity: "go1"}, Runner: runner}
	_, err := exec.Execute(context.Background(), root, testContract())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlinked artifact parent was accepted: %v", err)
	}
	if runner.calls != 0 {
		t.Fatalf("symlinked artifact parent reached command execution: calls=%d", runner.calls)
	}
	if _, err := os.Stat(filepath.Join(outside, "bootstrap")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("bootstrap wrote through symlinked parent: %v", err)
	}
}

func hasEnvWorktreeArtifact(env []string, key, rel string) bool {
	prefix := key + "="
	suffix := string(filepath.Separator) + filepath.FromSlash(rel)
	for _, value := range env {
		if strings.HasPrefix(value, prefix) && strings.HasSuffix(value, suffix) {
			return true
		}
	}
	return false
}
