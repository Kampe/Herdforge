package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMisdirectedPoolRootIsReported is the FAC-577 gate.
//
// The flag is the POOL DIRECTORY but was named --root, which reads as
// "repository root". A caller passing `--root .` pointed the pool at the working
// directory, so `release <lease>` answered "lease not found" for a lease that was
// plainly held, and `list` printed nothing while pool.json held two slots.
//
// "Not found" reads as "already released". That is how a consumer concluded a
// warm slot was free while it was still leased.
func TestMisdirectedPoolRootIsReported(t *testing.T) {
	repo := t.TempDir()
	nested := filepath.Join(repo, ".herd", "pool")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "pool.json"), []byte(`{"version":1,"slots":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	hint, misdirected := misdirectedPoolRoot(repo)
	if !misdirected {
		t.Fatal("a repository root passed as the pool root must be reported, not silently searched")
	}
	if hint != nested {
		t.Errorf("hint = %q, want the real pool directory %q", hint, nested)
	}
}

// The real pool directory must never be flagged.
func TestCorrectPoolRootIsNotFlagged(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pool.json"), []byte(`{"version":1,"slots":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, misdirected := misdirectedPoolRoot(dir); misdirected {
		t.Error("a directory holding real pool state must not be flagged")
	}
}

// A previous misuse leaves an empty pool.json wherever it was pointed, and that
// file then makes the mistake look legitimate forever. When BOTH exist the
// operator must be told which is real — I created exactly such a stray file in
// this repository by making this mistake myself.
func TestStrayPoolStateStillReportsTheRealPool(t *testing.T) {
	repo := t.TempDir()
	nested := filepath.Join(repo, ".herd", "pool")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(nested, "pool.json"), filepath.Join(repo, "pool.json")} {
		if err := os.WriteFile(p, []byte(`{"version":1,"slots":[]}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hint, misdirected := misdirectedPoolRoot(repo)
	if !misdirected {
		t.Fatal("a stray pool.json must not make a misdirected root look legitimate")
	}
	if hint != nested {
		t.Errorf("hint = %q, want %q", hint, nested)
	}
}

// Nothing underneath means nothing to suggest, so an ordinary empty directory
// must not be flagged.
func TestUnrelatedDirectoryIsNotFlagged(t *testing.T) {
	if _, misdirected := misdirectedPoolRoot(t.TempDir()); misdirected {
		t.Error("a directory with no pool anywhere must not be flagged")
	}
	if _, misdirected := misdirectedPoolRoot(""); misdirected {
		t.Error("an empty path must not be flagged")
	}
}
