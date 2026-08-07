package freshbuild

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveTarget_PathToName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "packages", "a")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"name":"@scope/a"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	pkg, isPath, err := ResolveTarget(root, filepath.Join("packages", "a"))
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "@scope/a" || !isPath {
		t.Fatalf("got pkg=%q isPath=%v", pkg, isPath)
	}
}

func TestResolveTarget_PathNoName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	pkgDir := filepath.Join(root, "packages", "b")
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "package.json"), []byte(`{"scripts":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, err := ResolveTarget(root, filepath.Join("packages", "b"))
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if !strings.Contains(err.Error(), "package.json has no name") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolveTarget_BareName(t *testing.T) {
	t.Parallel()
	pkg, isPath, err := ResolveTarget(t.TempDir(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	if pkg != "foo" || isPath {
		t.Fatalf("got pkg=%q isPath=%v", pkg, isPath)
	}
}

func TestChainSections_RelativeAndSorted(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	a := filepath.Join(root, "pkgs", "a")
	b := filepath.Join(root, "pkgs", "b")
	dirs, err := normalizeChainDirs(root, []string{b, a, a, b})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 2 {
		t.Fatalf("dirs=%v", dirs)
	}
	c := &Chain{Target: "x", Dirs: dirs}
	secs := c.Sections(root)
	if len(secs) != 2 || secs[0] != "pkgs/a" || secs[1] != "pkgs/b" {
		t.Fatalf("sections=%v", secs)
	}
}

func TestNormalizeChainDirs_RejectsEscape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	outside := filepath.Join(root, "..", "escape")
	_, err := normalizeChainDirs(root, []string{outside})
	if err == nil {
		t.Fatal("expected escape rejection")
	}
}

func TestDetectProfile_OrderAndHonesty(t *testing.T) {
	t.Parallel()

	pnpmRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pnpmRoot, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(pnpmRoot); p == nil || p.Name() != "pnpm" {
		t.Fatalf("pnpm profile: %+v", p)
	}

	// Go wins over incidental package.json (docs tooling).
	goWithPkg := t.TempDir()
	if err := os.WriteFile(filepath.Join(goWithPkg, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(goWithPkg, "package.json"), []byte(`{"name":"docs"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(goWithPkg); p == nil || p.Name() != "go" {
		t.Fatalf("go+package.json must select go, got %+v", p)
	}

	// package.json alone without packageManager → refuse.
	bare := t.TempDir()
	if err := os.WriteFile(filepath.Join(bare, "package.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(bare); p != nil {
		t.Fatalf("ambiguous package.json must refuse, got %s", p.Name())
	}

	// packageManager=pnpm → pnpm.
	pm := t.TempDir()
	if err := os.WriteFile(filepath.Join(pm, "package.json"), []byte(`{"name":"x","packageManager":"pnpm@9.0.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(pm); p == nil || p.Name() != "pnpm" {
		t.Fatalf("packageManager pnpm: %+v", p)
	}

	// npm lock → refuse.
	npm := t.TempDir()
	if err := os.WriteFile(filepath.Join(npm, "package.json"), []byte(`{"name":"x"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(npm, "package-lock.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(npm); p != nil {
		t.Fatalf("npm lock must refuse, got %s", p.Name())
	}

	empty := t.TempDir()
	if p := DetectProfile(empty); p != nil {
		t.Fatalf("empty root should not detect: %v", p.Name())
	}
}

func TestGoResolveTarget_RootRelativeNotCwd(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// Create root/cmd and a cwd-local cmd that would confuse cwd-relative stat.
	rootCmd := filepath.Join(root, "cmd")
	if err := os.MkdirAll(rootCmd, 0o755); err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cwd, "cmd"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Change into cwd so a naive os.Stat("cmd") would see the wrong dir.
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	g := GoProfile{}
	pkg, isPath, err := g.ResolveTarget(root, "cmd")
	if err != nil {
		t.Fatal(err)
	}
	if !isPath || pkg != "./cmd" {
		t.Fatalf("pkg=%q isPath=%v", pkg, isPath)
	}
	dirs, err := g.ChainFor(nil, root, pkg)
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != rootCmd {
		t.Fatalf("chain must be root/cmd, got %v (cwd cmd=%s)", dirs, filepath.Join(cwd, "cmd"))
	}
}

func TestPnpmMessagesClaimStaleDist_GoMessagesDoNot(t *testing.T) {
	t.Parallel()
	p := PnpmProfile{}
	if !strings.Contains(p.CleanLine(2), "STALE DIST") {
		t.Fatalf("pnpm clean must claim STALE DIST: %s", p.CleanLine(2))
	}
	if !strings.Contains(p.DryRunClearLine(), "dist/") {
		t.Fatalf("pnpm dry-run must mention dist: %s", p.DryRunClearLine())
	}
	g := GoProfile{}
	if strings.Contains(g.CleanLine(1), "STALE DIST") {
		t.Fatalf("go clean must not claim STALE DIST: %s", g.CleanLine(1))
	}
	if strings.Contains(g.DryRunClearLine(), "dist/") && !strings.Contains(g.DryRunClearLine(), "nothing") {
		t.Fatalf("go dry-run must not promise dist clear: %s", g.DryRunClearLine())
	}
	if !strings.Contains(g.DryRunClearLine(), "nothing") {
		t.Fatalf("go dry-run must say nothing cleared: %s", g.DryRunClearLine())
	}
	if strings.Contains(g.ClearedLine(1), "cleared dist") {
		t.Fatalf("go cleared line must not claim dist clear: %s", g.ClearedLine(1))
	}
}
