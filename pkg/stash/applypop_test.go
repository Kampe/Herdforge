package stash

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyEmpty(t *testing.T) {
	r, _ := newRepo(t)
	_, err := r.ApplyKeep(context.Background())
	if !errors.Is(err, ErrNoEntries) {
		t.Fatalf("want ErrNoEntries, got %v", err)
	}
	_, err = r.Pop(context.Background())
	if !errors.Is(err, ErrNoEntries) {
		t.Fatalf("want ErrNoEntries, got %v", err)
	}
}

// TestApplyConflictKeepsEntry forces a real three-way conflict on apply and
// asserts the entry is KEPT with the recover error text. Soft-success is a
// hard failure of this test (not a skip): the named claim is conflict keep.
func TestApplyConflictKeepsEntry(t *testing.T) {
	r, dir := newRepo(t)

	// Multi-line base so the stash patch and a diverging commit share context
	// but disagree on the middle line — the classic 3-way conflict shape.
	base := "alpha\nbase-line\ngamma\n"
	stashed := "alpha\nstashed-line\ngamma\n"
	diverged := "alpha\ndiverged-line\ngamma\n"

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "f.txt")
	run(t, dir, "commit", "-q", "-m", "multiline-base")

	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(stashed), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	ref, err := r.PushOpts(context.Background(), PushOptions{Message: "c", Stderr: &buf})
	if err != nil {
		t.Fatal(err)
	}
	// Worktree is clean at base again. Commit a diverging edit of the same line.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(diverged), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "add", "f.txt")
	run(t, dir, "commit", "-q", "-m", "diverge")

	_, err = r.ApplyKeep(context.Background())
	if err == nil {
		t.Fatal("expected apply conflict: clean diverging commit must not apply stashed middle line cleanly")
	}
	if !strings.Contains(err.Error(), "KEPT") {
		t.Fatalf("conflict error must mention KEPT, got %v", err)
	}
	if !strings.Contains(err.Error(), ref) {
		t.Fatalf("conflict error must name the entry, got %v", err)
	}
	// Conflict path must never drop the ref.
	refs, listErr := r.Entries()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(refs) != 1 || refs[0] != ref {
		t.Fatalf("entry must remain after conflict, got %v", refs)
	}
	// Pop after a failed apply must also keep the entry (same apply path).
	_, popErr := r.Pop(context.Background())
	if popErr == nil {
		t.Fatal("pop over the same conflict must also fail")
	}
	if !strings.Contains(popErr.Error(), "KEPT") {
		t.Fatalf("pop conflict error must mention KEPT, got %v", popErr)
	}
	refs, listErr = r.Entries()
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(refs) != 1 || refs[0] != ref {
		t.Fatalf("entry must remain after failed pop, got %v", refs)
	}
}

func TestPopDropsNewest(t *testing.T) {
	r, dir := newRepo(t)
	var buf bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("p", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.PushOpts(context.Background(), PushOptions{Message: "p", Stderr: &buf}); err != nil {
			t.Fatal(err)
		}
	}
	refsBefore, _ := r.Entries()
	if len(refsBefore) != 2 {
		t.Fatalf("want 2 entries, got %v", refsBefore)
	}
	// Pop newest only.
	ref, err := r.Pop(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(ref, "/1") {
		t.Fatalf("must pop newest (/1), got %q", ref)
	}
	refsAfter, _ := r.Entries()
	if len(refsAfter) != 1 || !strings.HasSuffix(refsAfter[0], "/0") {
		t.Fatalf("only /0 must remain, got %v", refsAfter)
	}
}

func TestListNewestFirst(t *testing.T) {
	r, dir := newRepo(t)
	var buf bytes.Buffer
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("L", i+1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.PushOpts(context.Background(), PushOptions{Message: "list-msg", Stderr: &buf}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := r.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2, got %d", len(entries))
	}
	if !strings.HasSuffix(entries[0].Ref, "/1") || !strings.HasSuffix(entries[1].Ref, "/0") {
		t.Fatalf("newest first required, got %v then %v", entries[0].Ref, entries[1].Ref)
	}
	if !strings.Contains(entries[0].Summary, "list-msg") {
		t.Fatalf("summary should include subject, got %q", entries[0].Summary)
	}
	_ = dir
}
