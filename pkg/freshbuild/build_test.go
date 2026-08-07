package freshbuild

import (
	"os"
	"path/filepath"
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
	pkg, isPath, err := ResolveTarget(root, pkgDir)
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
	_, _, err := ResolveTarget(root, pkgDir)
	if err == nil {
		t.Fatal("expected error for missing name")
	}
	if want := "package.json has no name"; !contains(err.Error(), want) {
		t.Fatalf("err=%v want substring %q", err, want)
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
	// Intentionally reverse input order; normalize sorts.
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

func TestDetectProfile_PnpmAndGo(t *testing.T) {
	t.Parallel()
	pnpmRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(pnpmRoot, "pnpm-lock.yaml"), []byte("lockfileVersion: '9.0'\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(pnpmRoot); p == nil || p.Name() != "pnpm" {
		t.Fatalf("pnpm profile: %+v", p)
	}

	goRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(goRoot, "go.mod"), []byte("module example.com/x\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := DetectProfile(goRoot); p == nil || p.Name() != "go" {
		t.Fatalf("go profile: %+v", p)
	}

	empty := t.TempDir()
	if p := DetectProfile(empty); p != nil {
		t.Fatalf("empty root should not detect: %v", p.Name())
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 ||
		(len(s) > 0 && (func() bool {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
			return false
		})()))
}
