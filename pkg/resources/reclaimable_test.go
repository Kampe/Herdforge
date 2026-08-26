package resources

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path string, n int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, n), 0o644); err != nil {
		t.Fatal(err)
	}
}

// FAC-654: the gate refused with next_action=recover_capacity_without_cleanup
// and nothing else, while 19.5 GiB of rebuildable cache sat on the same
// filesystem. An operator told only "below_threshold" reaches for the largest
// visible thing, which is worktrees -- exactly the wrong place, since a worktree
// can hold uncommitted work or an unmerged branch.
func TestScanReclaimableReportsCachesLargestFirst(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "Library", "Caches", "go-build", "a"), 3000)
	write(t, filepath.Join(home, ".npm", "_npx", "b"), 9000)

	got := ScanReclaimable(home)
	if len(got) < 2 {
		t.Fatalf("expected both caches, got %+v", got)
	}
	if got[0].Bytes < got[1].Bytes {
		t.Errorf("classes must be largest-first so the useful action is first: %+v", got)
	}
	for _, c := range got {
		if c.Rebuild == "" {
			t.Errorf("every class must say how it is rebuilt, or it reads as data loss: %+v", c)
		}
	}
}

// The single largest real win was a DEAD pnpm store generation: 6.9 GiB in v10
// while the active v11 held 66 MB. `pnpm store prune` cannot see it, because it
// only prunes within the active generation.
func TestScanReclaimableFindsDeadPnpmGenerationsButNotTheActiveOne(t *testing.T) {
	home := t.TempDir()
	base := filepath.Join(home, "Library", "pnpm", "store")
	write(t, filepath.Join(base, "v10", "old"), 8000)
	write(t, filepath.Join(base, "v11", "current"), 100)

	got := ScanReclaimable(home)
	var sawDead, sawActive bool
	for _, c := range got {
		if filepath.Base(c.Path) == "v10" {
			sawDead = true
		}
		if filepath.Base(c.Path) == "v11" {
			sawActive = true
		}
	}
	if !sawDead {
		t.Error("a superseded pnpm generation is pure garbage and must be reported")
	}
	if sawActive {
		t.Error("the ACTIVE store generation must never be offered for deletion")
	}
}

// A single generation is the normal case and has nothing dead in it.
func TestScanReclaimableLeavesASoleStoreGenerationAlone(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "Library", "pnpm", "store", "v11", "only"), 5000)
	for _, c := range ScanReclaimable(home) {
		if filepath.Base(c.Path) == "v11" {
			t.Fatal("with one generation there is nothing superseded; it must not be listed")
		}
	}
}

// Fleet state must never be suggested. Worktrees, leases and ledgers can hold
// unique work; offering them to satisfy a capacity gate trades correctness for
// space, which is the operator's decision and not a gate's hint.
func TestScanReclaimableNeverSuggestsFleetState(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "Personal", "proj", ".herd", "worktrees", "cha-1", "f"), 50000)
	write(t, filepath.Join(home, "Personal", "proj", ".herd", "pool", "pool-01", "f"), 50000)
	write(t, filepath.Join(home, "Library", "Caches", "go-build", "a"), 100)

	for _, c := range ScanReclaimable(home) {
		if filepath.Base(c.Path) != "go-build" {
			t.Errorf("only caches may be reported, got %q", c.Path)
		}
	}
}

// FAC-654: the walk runs inside a FAIL-CLOSED admission gate, and a build cache
// holds hundreds of thousands of small files. An unbounded walk makes the gate
// slow exactly when the disk is full and the operator is already stuck, so it
// stops early and reports a floor. Under-counting is the safe direction: it can
// never talk an operator into deleting something the scan did not verify.
func TestScanReclaimableIsBoundedAndUnderCounts(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "Caches", "go-build")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	const files = 64
	for i := 0; i < files; i++ {
		write(t, filepath.Join(dir, "f", string(rune('a'+i%26))+string(rune('a'+i/26))), 1000)
	}
	got := ScanReclaimable(home)
	if len(got) == 0 {
		t.Fatal("a populated cache must be reported")
	}
	if got[0].Bytes == 0 {
		t.Error("a bounded scan must still report a usable floor, not zero")
	}
	if maxScanFiles <= 0 {
		t.Fatal("the bound must be positive or the walk reports nothing")
	}
}
