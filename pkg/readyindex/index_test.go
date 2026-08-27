package readyindex

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUpsertRemoveAndList(t *testing.T) {
	path := filepath.Join(t.TempDir(), Leaf)
	if err := Upsert(path, Entry{SHA: "aaa", Branch: "feat/a", Reviewer: "r1"}); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, Entry{SHA: "bbb", Branch: "feat/b", Reviewer: "r2"}); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, Entry{SHA: "aaa", Branch: "feat/a2", Reviewer: "r1"}); err != nil {
		t.Fatal(err)
	}
	entries, err := List(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("len=%d want 2", len(entries))
	}
	if entries[0].SHA != "aaa" || entries[0].Branch != "feat/a2" {
		t.Fatalf("upsert did not refresh: %+v", entries[0])
	}
	if err := Remove(path, "aaa"); err != nil {
		t.Fatal(err)
	}
	entries, err = List(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].SHA != "bbb" {
		t.Fatalf("after remove: %+v", entries)
	}
}

func TestRebuildReplacesProjection(t *testing.T) {
	path := filepath.Join(t.TempDir(), Leaf)
	_ = Upsert(path, Entry{SHA: "stale"})
	if err := Rebuild(path, []Entry{{SHA: "fresh", Branch: "b"}}, "repair"); err != nil {
		t.Fatal(err)
	}
	idx, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if idx.Source != "repair" || len(idx.Entries) != 1 || idx.Entries[0].SHA != "fresh" {
		t.Fatalf("rebuild: %+v", idx)
	}
}

func TestMissingIndexIsNotExist(t *testing.T) {
	_, err := List(filepath.Join(t.TempDir(), Leaf))
	if !os.IsNotExist(err) {
		t.Fatalf("want ErrNotExist, got %v", err)
	}
}

func TestPathForBesideLedger(t *testing.T) {
	got := PathFor("/repo/.herd/review-ledger.jsonl")
	want := "/repo/.herd/" + Leaf
	if got != want {
		t.Fatalf("PathFor=%q want %q", got, want)
	}
}
