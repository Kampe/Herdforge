package harvest

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// TestMain disables the disk-pressure floors so existing integration tests
// stay hermetic on a pressured host (FAC-153 incident host sat at 99%).
// Guard-assertion tests re-enable floors via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(preflight.EnvDiskMinFreeGB, "0")
	os.Setenv(preflight.EnvDiskMinFreePct, "0")
	os.Setenv(preflight.EnvDiskMinInodePct, "0")
	os.Exit(m.Run())
}

func TestIntegrationRunRefusesUnderDiskPressure(t *testing.T) {
	// 1 ZiB floor: any real volume reads as critically low.
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")

	root, _ := setupRepoWithRemote(t)
	wt := createWorktree(t, root, "task/FAC-300-p")
	writeFileHarvest(t, wt, "p.go", "package p")
	addAndCommitHarvest(t, wt, "feat: FAC-300 p", "p.go")

	l := setupLedger(t, root)
	in := NewIntegration(NewHarvester(root), nil, &recordingDispatcher{}, l, root)
	res, err := in.Run(context.Background())
	if err == nil {
		t.Fatal("expected fail-closed refusal under disk pressure")
	}
	if !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected structured disk_pressure evidence, got: %v", err)
	}
	if res != nil {
		t.Fatalf("expected nil result on refusal, got %+v", res)
	}
	// Refused before any mutation: the candidate worktree is untouched.
	if _, statErr := os.Stat(wt); statErr != nil {
		t.Fatalf("candidate worktree disturbed: %v", statErr)
	}
}

func TestIntegrationRunProbesCandidateWorktreeVolumes(t *testing.T) {
	// A pool on a different filesystem must be probed, not assumed healthy
	// because the repo volume is. Swap the default guard for one whose
	// prober records every probed path and assert each candidate worktree
	// path is included in the capacity decision.
	root, _ := setupRepoWithRemote(t)
	wt := createWorktree(t, root, "task/FAC-301-q")
	writeFileHarvest(t, wt, "q.go", "package q")
	addAndCommitHarvest(t, wt, "feat: FAC-301 q", "q.go")

	var probed []string
	orig := preflight.DefaultDiskGuard
	preflight.DefaultDiskGuard = preflight.NewDiskGuard(func(path string) (preflight.DiskStat, error) {
		probed = append(probed, path)
		return preflight.DiskStat{Path: path, FSID: path, TotalBytes: 1 << 40, FreeBytes: 1 << 39,
			FreePct: 50, TotalInodes: 1 << 20, FreeInodes: 1 << 19, InodeFreePct: 50}, nil
	})
	defer func() { preflight.DefaultDiskGuard = orig }()

	l := setupLedger(t, root)
	in := NewIntegration(NewHarvester(root), nil, &recordingDispatcher{}, l, root, WithDryRun(true))
	if _, err := in.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	found := false
	for _, p := range probed {
		if strings.Contains(p, "FAC-301") {
			found = true
		}
	}
	if !found {
		t.Fatalf("candidate worktree volume never probed; probed=%v", probed)
	}
}

func TestIntegrationRunShedsBatchUnderSoftPressure(t *testing.T) {
	// Reserve floors stay zeroed (TestMain) so the hard gate passes on any
	// host; a saturated soft floor puts every real volume in the soft band.
	t.Setenv(preflight.EnvDiskSerializeFreeGB, "1099511627776")

	ctx := context.Background()
	root, _ := setupRepoWithRemote(t)
	wt1 := createWorktree(t, root, "task/FAC-201-a")
	writeFileHarvest(t, wt1, "a.go", "package a")
	addAndCommitHarvest(t, wt1, "feat: FAC-201 a", "a.go")
	wt2 := createWorktree(t, root, "task/FAC-202-b")
	writeFileHarvest(t, wt2, "b.go", "package b")
	addAndCommitHarvest(t, wt2, "feat: FAC-202 b", "b.go")

	l := setupLedger(t, root)
	in := NewIntegration(NewHarvester(root), nil, &recordingDispatcher{}, l, root, WithDryRun(true))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.DiskAdvice != preflight.AdviceSerialize {
		t.Fatalf("disk advice = %q, want serialize", res.DiskAdvice)
	}
	// Batch shed to exactly one candidate worktree; the deferral is
	// counted, never silent.
	if len(res.HarvestResult.UnmergedWorktrees) != 1 {
		t.Fatalf("expected batch shed to 1 worktree, got %d", len(res.HarvestResult.UnmergedWorktrees))
	}
	if res.ShedWorktrees != 1 {
		t.Fatalf("shed count = %d, want 1", res.ShedWorktrees)
	}

	// Both deferred worktrees remain fully intact for the next run.
	for _, wt := range []string{wt1, wt2} {
		if _, err := os.Stat(wt); err != nil {
			t.Fatalf("worktree %s disturbed: %v", wt, err)
		}
	}
}
