package overlap

import (
	"context"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestFileOverlaps_NoOverlap(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nmore\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "alpha")
	commitFile(t, dir, "alone.go", "package a\n", "alpha work")

	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	hots, scanned, err := o.FileOverlaps(context.Background(), "origin/main", 2, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	if scanned != 1 {
		t.Fatalf("expected scanned=1, got %d", scanned)
	}
	if len(hots) != 0 {
		t.Fatalf("expected no overlap, got %v", hots)
	}
}

func TestFileOverlap_TwoTipsOneFile(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\n\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "alpha")
	commitFile(t, dir, "file.go", "package x\nfunc A(){}\n", "alpha edit")

	checkoutBranch(t, dir, "main")
	createBranch(t, dir, "beta")
	commitFile(t, dir, "file.go", "package x\nfunc B(){}\n", "beta edit")

	checkoutBranch(t, dir, "main")

	o := NewOverlap(dir)
	hots, scanned, err := o.FileOverlaps(context.Background(), "origin/main", 2, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	if scanned != 2 {
		t.Fatalf("expected scanned=2, got %d", scanned)
	}
	if len(hots) != 1 {
		t.Fatalf("expected 1 hot file, got %v", hots)
	}
	got := hots[0]
	if got.File != "file.go" {
		t.Fatalf("expected file.go, got %s", got.File)
	}
	if len(got.Branches) != 2 {
		t.Fatalf("expected 2 owners, got %v", got.Branches)
	}
	if !slices.Contains(got.Branches, "alpha") || !slices.Contains(got.Branches, "beta") {
		t.Fatalf("owners missing alpha/beta: %v", got.Branches)
	}
}

func TestOverlap_ParkTriplicate(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nx\n", "advance main")
	setOriginMain(t, dir, "")

	createBranch(t, dir, "work-1")
	commitFile(t, dir, "file.go", "package x\n", "work")

	// Same tip surfaced as three refs: -submit, -<short-sha>, -<full-sha>.
	workTip := gitRun(t, dir, "rev-parse", "work-1")
	checkoutBranch(t, dir, "main")
	createBranch(t, dir, "work-current")
	forceRev(t, dir, "work-1")
	createBranch(t, dir, "work-"+workTip[:8])
	createBranch(t, dir, "work-"+workTip)

	if len(listBranches(t, dir)) != 5 {
		t.Fatalf("expected 5 branches, got %v", listBranches(t, dir))
	}

	o := NewOverlap(dir)
	hots, scanned, err := o.FileOverlaps(context.Background(), "origin/main", 2, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	// Three refs, one distinct tip: only the first branch for that tip
	// participates and only that one can own the file.
	if scanned != 1 {
		t.Fatalf("expected scanned=1 distinct tip, got %d", scanned)
	}
	if len(hots) != 0 {
		t.Fatalf("one tip must not manufacture an overlap, got %v", hots)
	}
}

func TestOverlap_ExclusionsRegistry(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\ny\n", "advance main")
	setOriginMain(t, dir, "")

	for _, b := range []string{"a1", "a2", "a3", "a4", "a5"} {
		createBranch(t, dir, b)
		commitFile(t, dir, "pnpm-lock.yaml", "lock "+b+"\n", b)
		commitFile(t, dir, "AGENTS.md", "agents "+b+"\n", b)
		commitFile(t, dir, "docs/QUALITY.md", "qual "+b+"\n", b)
		checkoutBranch(t, dir, "main")
	}

	o := NewOverlap(dir)
	hots, scanned, err := o.FileOverlaps(context.Background(), "origin/main", 2, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	if scanned != 5 {
		t.Fatalf("expected scanned=5, got %d", scanned)
	}
	if len(hots) != 0 {
		t.Fatalf("registry files touch each other by construction, got %v", hots)
	}
}

func TestOverlap_DeterministicRuns(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\nz\n", "advance main")
	setOriginMain(t, dir, "")
	for _, b := range []string{"bravo", "alpha", "charlie"} {
		createBranch(t, dir, b)
		commitFile(t, dir, "shared/file.go", "package x\n// "+b+"\n", b)
		commitFile(t, dir, "only-"+b+".go", "package x\n", b)
		checkoutBranch(t, dir, "main")
	}

	o := NewOverlap(dir)
	r1, s1, err := o.FileOverlaps(context.Background(), "origin/main", 2, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	r2, s2, err := o.FileOverlaps(context.Background(), "origin/main", 2, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	if s1 != s2 || s1 != 3 {
		t.Fatalf("scanned should be stable 3 (got %d then %d)", s1, s2)
	}
	a, _ := json.Marshal(r1)
	b, _ := json.Marshal(r2)
	if string(a) != string(b) {
		t.Fatalf("non-deterministic runs:\n%s\nvs\n%s", a, b)
	}
	if len(r1) != 1 {
		t.Fatalf("expected exactly shared/file.go hot, got %v", r1)
	}
	sort.Strings(r1[0].Branches)
	if strings.Join(r1[0].Branches, ",") != "alpha,bravo,charlie" {
		t.Fatalf("owners wrong: %v", r1[0].Branches)
	}
	if len(r2[0].Branches) != 3 {
		t.Fatalf("second run owners wrong: %v", r2[0].Branches)
	}
}

func TestOverlap_MinimumThree(t *testing.T) {
	dir := createTestGitRepo(t, "base.txt")
	commitFile(t, dir, "base.txt", "base\ny\n", "advance main")
	setOriginMain(t, dir, "")
	for _, b := range []string{"alpha", "beta"} {
		createBranch(t, dir, b)
		commitFile(t, dir, "file.go", "package x\n", b)
		checkoutBranch(t, dir, "main")
	}

	o := NewOverlap(dir)
	hots, scanned, err := o.FileOverlaps(context.Background(), "origin/main", 3, nil)
	if err != nil {
		t.Fatalf("FileOverlaps: %v", err)
	}
	if scanned != 2 {
		t.Fatalf("expected scanned=2, got %d", scanned)
	}
	if len(hots) != 0 {
		t.Fatalf("min 3 with only 2 tips must not be hot, got %v", hots)
	}
}