package freshbuild

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFreshBuild_DryRunDeletesNothing_PnpmMessages(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "pkgs", "a")
	if err := os.MkdirAll(filepath.Join(a, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(a, "dist", "keep.js")
	if err := os.WriteFile(marker, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "@scope/a",
		DryRun:   true,
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "/bin/pnpm", nil },
		ChainFn: func(ctx context.Context, root, pkg string) ([]string, error) {
			return []string{a}, nil
		},
		Stdout: &stdout,
		Stderr: &stderr,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictDryRun || v.Rc != 0 {
		t.Fatalf("verdict=%+v", v)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dry-run must not delete dist: %v", err)
	}
	out := stdout.String()
	if !strings.Contains(out, "chain for @scope/a = 1 package(s) (target + dependencies)") {
		t.Fatalf("pnpm plan header missing:\n%s", out)
	}
	if !strings.Contains(out, "would clear dist/") {
		t.Fatalf("pnpm dry-run must mention dist clear:\n%s", out)
	}
	if !strings.Contains(out, "Nothing changed.") {
		t.Fatalf("dry-run message missing:\n%s", out)
	}
}

func TestFreshBuild_DryRun_GoDoesNotPromiseDistClear(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "pkg", "x")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "./pkg/x",
		DryRun:   true,
		Profile:  GoProfile{},
		LookPath: func(string) (string, error) { return "/usr/bin/go", nil },
		Stdout:   &stdout,
		Stderr:   io.Discard,
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictDryRun {
		t.Fatalf("verdict=%+v", v)
	}
	out := stdout.String()
	if strings.Contains(out, "STALE DIST") {
		t.Fatalf("go dry-run must not claim STALE DIST:\n%s", out)
	}
	if strings.Contains(out, "would clear dist/") {
		t.Fatalf("go dry-run must not promise dist clear:\n%s", out)
	}
	if !strings.Contains(out, "nothing") {
		t.Fatalf("go dry-run must say clear nothing:\n%s", out)
	}
	if !strings.Contains(out, "target only") {
		t.Fatalf("go chain header must not claim dependencies:\n%s", out)
	}
	if strings.Contains(out, "target + dependencies") {
		t.Fatalf("go chain header must not claim dependencies:\n%s", out)
	}
}

func TestFreshBuild_CleanVerdict_PnpmClaimsStaleDist(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(filepath.Join(a, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(a, "dist", "x.js"), []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("@scope/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "a",
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "/bin/pnpm", nil },
		ChainFn: func(ctx context.Context, root, pkg string) ([]string, error) {
			return []string{a}, nil
		},
		Runner: func(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
			return 0, nil
		},
		Stdout:  &stdout,
		Stderr:  io.Discard,
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictClean || v.Rc != 0 {
		t.Fatalf("verdict=%+v", v)
	}
	if _, err := os.Stat(filepath.Join(a, "dist")); !os.IsNotExist(err) {
		t.Fatal("clean path must still clear dist before rebuild")
	}
	if !strings.Contains(stdout.String(), "STALE DIST, not real") {
		t.Fatalf("stdout=%s", stdout.String())
	}
	if v.LogPath != "" {
		t.Fatalf("clean must remove log, got %q", v.LogPath)
	}
}

func TestFreshBuild_CleanVerdict_GoDoesNotClaimStaleDist(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pkgDir := filepath.Join(root, "pkg", "x")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A dist that must NOT be deleted by Go profile.
	if err := os.MkdirAll(filepath.Join(pkgDir, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(pkgDir, "dist", "keep")
	if err := os.WriteFile(marker, []byte("1"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "./pkg/x",
		Profile:  GoProfile{},
		LookPath: func(string) (string, error) { return "/usr/bin/go", nil },
		Runner: func(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
			return 0, nil
		},
		Stdout:  &stdout,
		Stderr:  io.Discard,
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictClean {
		t.Fatalf("verdict=%+v", v)
	}
	out := stdout.String()
	if strings.Contains(out, "STALE DIST") {
		t.Fatalf("go clean must not claim STALE DIST:\n%s", out)
	}
	if strings.Contains(out, "cleared dist") {
		t.Fatalf("go must not claim cleared dist:\n%s", out)
	}
	if !strings.Contains(out, "does not diagnose stale dist") {
		t.Fatalf("go clean must disclaim stale-dist diagnosis:\n%s", out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("go profile must not delete dist: %v", err)
	}
}

func TestFreshBuild_NodeModulesVerdict(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("packages:\n  '@scope/x':\n    resolution: {integrity: sha}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "a",
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "/bin/pnpm", nil },
		ChainFn: func(ctx context.Context, root, pkg string) ([]string, error) {
			return []string{a}, nil
		},
		Runner: func(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
			fmt.Fprintln(log, "Cannot find module '@scope/x'")
			return 7, fmt.Errorf("exit 7")
		},
		Stdout:  io.Discard,
		Stderr:  &stderr,
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictNodeModules || v.Rc != 7 || v.MissingModule != "@scope/x" {
		t.Fatalf("verdict=%+v", v)
	}
	if !strings.Contains(stderr.String(), "STALE/MISSING node_modules") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "Run: pnpm install") {
		t.Fatalf("node_modules verdict must print remediation line:\n%s", stderr.String())
	}
}

func TestFreshBuild_RealErrorModuleNotInLock(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("packages: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "a",
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "/bin/pnpm", nil },
		ChainFn: func(ctx context.Context, root, pkg string) ([]string, error) {
			return []string{a}, nil
		},
		Runner: func(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
			fmt.Fprintln(log, "Cannot find module '@scope/nope'")
			fmt.Fprintln(log, "error TS2307: bad")
			return 7, fmt.Errorf("exit 7")
		},
		Stdout:  io.Discard,
		Stderr:  &stderr,
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictRealError || v.Rc != 7 {
		t.Fatalf("verdict=%+v", v)
	}
	if !strings.Contains(stderr.String(), "REAL build error") {
		t.Fatalf("stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "full log at") {
		t.Fatalf("real error must print log path handoff:\n%s", stderr.String())
	}
	if v.LogPath == "" {
		t.Fatal("real error must keep log path")
	}
}

func TestFreshBuild_RealErrorGeneric(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "a")
	if err := os.MkdirAll(a, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	v, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "a",
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "/bin/pnpm", nil },
		ChainFn: func(ctx context.Context, root, pkg string) ([]string, error) {
			return []string{a}, nil
		},
		Runner: func(ctx context.Context, root, pkg string, log io.Writer) (int, error) {
			fmt.Fprintln(log, "error TS2009: something failed")
			return 5, fmt.Errorf("exit 5")
		},
		Stdout:  io.Discard,
		Stderr:  &stderr,
		TempDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if v.Kind != VerdictRealError || v.Rc != 5 {
		t.Fatalf("verdict=%+v", v)
	}
	if !strings.Contains(stderr.String(), "error TS2009") {
		t.Fatalf("tail missing:\n%s", stderr.String())
	}
}

func TestFreshBuild_UsageNoTarget(t *testing.T) {
	t.Parallel()
	// Empty target returns before profile/lookpath are consulted — no fixtures.
	_, err := FreshBuild(context.Background(), Options{
		Root:   t.TempDir(),
		Target: "",
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err == nil {
		t.Fatal("expected usage error")
	}
	if !strings.Contains(err.Error(), "usage: herd fresh-build") {
		t.Fatalf("err=%v", err)
	}
}

func TestFreshBuild_PnpmRequired(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "a",
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "", fmt.Errorf("not found") },
		Stdout:   io.Discard,
		Stderr:   io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "pnpm required") {
		t.Fatalf("err=%v", err)
	}
}

func TestFreshBuild_EmptyChain(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	_, err := FreshBuild(context.Background(), Options{
		Root:     root,
		Target:   "missing",
		Profile:  PnpmProfile{},
		LookPath: func(string) (string, error) { return "/bin/pnpm", nil },
		ChainFn: func(ctx context.Context, root, pkg string) ([]string, error) {
			return nil, nil
		},
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err == nil || !strings.Contains(err.Error(), "no packages matched") {
		t.Fatalf("err=%v", err)
	}
}

func TestPnpmChainFor_FailClosedOnNonZero(t *testing.T) {
	t.Parallel()
	// Inject a fake pnpm that writes partial stdout then exits 1.
	dir := t.TempDir()
	script := filepath.Join(dir, "pnpm")
	// shell script: echo one dir, exit 1
	body := "#!/bin/sh\necho /tmp/partial-only\nexit 1\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	p := PnpmProfile{Pnpm: script}
	_, err := p.ChainFor(context.Background(), dir, "@scope/x")
	if err == nil {
		t.Fatal("expected fail-closed chain error")
	}
	if !strings.Contains(err.Error(), "chain resolution failed") && !strings.Contains(err.Error(), "pnpm") {
		t.Fatalf("err=%v", err)
	}
}

func TestPnpmClassifyFailure(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "pnpm-lock.yaml"), []byte("has @scope/x inside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := PnpmProfile{}
	kind, mod := p.ClassifyFailure(root, []byte("Cannot find module '@scope/x'\n"), 1)
	if kind != VerdictNodeModules || mod != "@scope/x" {
		t.Fatalf("kind=%s mod=%s", kind, mod)
	}
	kind, mod = p.ClassifyFailure(root, []byte("Cannot find module '@scope/nope'\n"), 1)
	if kind != VerdictRealError || mod != "@scope/nope" {
		t.Fatalf("kind=%s mod=%s", kind, mod)
	}
	kind, mod = p.ClassifyFailure(root, []byte("error TS2009\n"), 1)
	if kind != VerdictRealError || mod != "" {
		t.Fatalf("kind=%s mod=%s", kind, mod)
	}
}

func TestErrorTail_FilterAndFallback(t *testing.T) {
	t.Parallel()
	log := []byte("info ok\nerror TS1: a\nwarn\nFAILED here\nnoise\n")
	got := errorTail(log, 20)
	if len(got) != 2 {
		t.Fatalf("got=%v", got)
	}
	got = errorTail([]byte("one\ntwo\nthree\n"), 2)
	if len(got) != 2 || got[0] != "two" || got[1] != "three" {
		t.Fatalf("got=%v", got)
	}
}
