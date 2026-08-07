package verifier

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMarkedResidualsReapExactHolderAfterTransientAndTerminalScans(t *testing.T) {
	leader := procToken{pid: 4241, startSec: 11, startUsec: 1}
	holder := procToken{pid: 4242, startSec: 22, startUsec: 2}
	self := procToken{pid: os.Getpid(), startSec: 33, startUsec: 3}
	tests := []struct {
		name  string
		scans []struct {
			tokens []procToken
			err    error
		}
		wantReapCalls int
	}{
		{
			name: "transient empty then deadline",
			scans: []struct {
				tokens []procToken
				err    error
			}{
				{},
				{tokens: []procToken{holder}},
				{err: errors.New("marker scan deadline")},
			},
			wantReapCalls: 3,
		},
		{
			name: "permission after holder",
			scans: []struct {
				tokens []procToken
				err    error
			}{
				{tokens: []procToken{holder}},
				{err: fmt.Errorf("wrapped permission: %w", syscall.EPERM)},
			},
			wantReapCalls: 2,
		},
		{
			name: "deadline after holder",
			scans: []struct {
				tokens []procToken
				err    error
			}{
				{tokens: []procToken{holder}},
				{err: context.DeadlineExceeded},
			},
			wantReapCalls: 2,
		},
		{
			name: "exact exclusion",
			scans: []struct {
				tokens []procToken
				err    error
			}{
				{tokens: []procToken{leader, self, holder}},
				{err: errors.New("final marker scan error")},
			},
			wantReapCalls: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			previousDrain := markerLineageDrainedFn
			previousScan := markerLineageScanFn
			previousNote := markerTokenNoteFn
			previousLive := markerTokenLiveFn
			previousLeaderLive := markerLeaderLiveFn
			previousReap := markerResidualReapFn
			previousExclude := residualExcludePIDsFn
			t.Cleanup(func() {
				markerLineageDrainedFn = previousDrain
				markerLineageScanFn = previousScan
				markerTokenNoteFn = previousNote
				markerTokenLiveFn = previousLive
				markerLeaderLiveFn = previousLeaderLive
				markerResidualReapFn = previousReap
				residualExcludePIDsFn = previousExclude
			})

			owned := &ownedSubprocess{
				leader:     leader.pid,
				markerPath: "fixture-marker",
				handles:    map[int]ownedHandle{leader.pid: {tok: leader}},
			}
			markerLineageDrainedFn = func(string) (bool, error) { return false, nil }
			residualExcludePIDsFn = func() map[int]struct{} {
				return map[int]struct{}{self.pid: {}}
			}
			markerTokenLiveFn = func(procToken) bool { return true }
			markerLeaderLiveFn = func(ownedHandle) bool { return true }
			noted := make([]procToken, 0)
			markerTokenNoteFn = func(_ *ownedSubprocess, tok procToken) error {
				noted = append(noted, tok)
				return nil
			}
			reaped := make([][]procToken, 0)
			markerResidualReapFn = func(_ *ownedSubprocess) error {
				snapshot := append([]procToken(nil), noted...)
				reaped = append(reaped, snapshot)
				return nil
			}
			scanIndex := 0
			markerLineageScanFn = func(string, time.Time) ([]procToken, error) {
				if scanIndex >= len(tt.scans) {
					return nil, errors.New("unexpected extra marker scan")
				}
				scan := tt.scans[scanIndex]
				scanIndex++
				return scan.tokens, scan.err
			}

			if err := owned.adoptAndKillMarkedResiduals(); err == nil {
				t.Fatal("terminal marker scan error must remain BLOCKED")
			}
			if len(reaped) != tt.wantReapCalls {
				t.Fatalf("reap calls=%d, want %d; snapshots=%+v", len(reaped), tt.wantReapCalls, reaped)
			}
			if len(reaped[len(reaped)-1]) != 1 || !reaped[len(reaped)-1][0].equal(holder) {
				t.Fatalf("final reap=%+v, want exact holder %+v", reaped[len(reaped)-1], holder)
			}
			if len(noted) != 1 || !noted[0].equal(holder) {
				t.Fatalf("noted tokens=%+v, want only exact holder %+v", noted, holder)
			}
		})
	}
}

func TestMarkedResidualsRetriesTransientExactReapFailure(t *testing.T) {
	leader := procToken{pid: 4241, startSec: 11, startUsec: 1}
	holder := procToken{pid: 4242, startSec: 22, startUsec: 2}
	self := os.Getpid()
	transient := errors.New("transient exact reap failure")
	previousDrain := markerLineageDrainedFn
	previousScan := markerLineageScanFn
	previousNote := markerTokenNoteFn
	previousLive := markerTokenLiveFn
	previousLeaderLive := markerLeaderLiveFn
	previousReap := markerResidualReapFn
	previousExclude := residualExcludePIDsFn
	t.Cleanup(func() {
		markerLineageDrainedFn = previousDrain
		markerLineageScanFn = previousScan
		markerTokenNoteFn = previousNote
		markerTokenLiveFn = previousLive
		markerLeaderLiveFn = previousLeaderLive
		markerResidualReapFn = previousReap
		residualExcludePIDsFn = previousExclude
	})

	owned := &ownedSubprocess{
		leader:     leader.pid,
		markerPath: "fixture-marker",
		handles:    map[int]ownedHandle{leader.pid: {tok: leader}},
	}
	markerLineageDrainedFn = func(string) (bool, error) { return false, nil }
	scanCalls := 0
	markerLineageScanFn = func(string, time.Time) ([]procToken, error) {
		if scanCalls == 0 {
			scanCalls++
			return []procToken{leader, {pid: self, startSec: 33, startUsec: 3}, holder}, nil
		}
		return nil, errors.New("unexpected extra marker scan")
	}
	residualExcludePIDsFn = func() map[int]struct{} { return map[int]struct{}{self: {}} }
	markerTokenLiveFn = func(procToken) bool { return true }
	markerLeaderLiveFn = func(ownedHandle) bool { return true }
	noted := make([]procToken, 0)
	markerTokenNoteFn = func(_ *ownedSubprocess, tok procToken) error {
		noted = append(noted, tok)
		return nil
	}
	var attempts [][]procToken
	markerResidualReapFn = func(_ *ownedSubprocess) error {
		attempts = append(attempts, append([]procToken(nil), noted...))
		if len(attempts) == 1 {
			return transient
		}
		return nil
	}

	err := owned.adoptAndKillMarkedResiduals()
	if err == nil || !errors.Is(err, transient) {
		t.Fatalf("retry failure must preserve transient cause: %v", err)
	}
	if len(attempts) != 2 {
		t.Fatalf("exact reap attempts=%d, want 2: %+v", len(attempts), attempts)
	}
	for i, attempt := range attempts {
		if len(attempt) != 1 || !attempt[0].equal(holder) {
			t.Fatalf("attempt %d authority=%+v, want only holder %+v", i+1, attempt, holder)
		}
	}
}

// TestHermeticGitConfigFlagsReachGit is the non-vacuous coverage for
// hermeticGitConfig: git must resolve the -c overrides on the same argv path
// runGit uses. Deleting hermeticGitConfig fails these equality checks.
func TestHermeticGitConfigFlagsReachGit(t *testing.T) {
	root, err := os.MkdirTemp("", "verifier-hermetic-flags-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil && !os.IsNotExist(err) {
			t.Errorf("cleanup hermetic root: %v", err)
		}
	})

	if _, err := runGit(root, "init", "-q", "-b", "main"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "gc.auto", "6700"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "gc.autoDetach", "true"); err != nil {
		t.Fatal(err)
	}
	if _, err := runGit(root, "config", "maintenance.auto", "true"); err != nil {
		t.Fatal(err)
	}

	gotAuto, err := runGit(root, "config", "--get", "gc.auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotAuto)) != "0" {
		t.Fatalf("gc.auto via hermetic runGit = %q, want 0 (hermeticGitConfig must reach git)", strings.TrimSpace(string(gotAuto)))
	}
	gotDetach, err := runGit(root, "config", "--get", "gc.autoDetach")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotDetach)) != "false" {
		t.Fatalf("gc.autoDetach via hermetic runGit = %q, want false", strings.TrimSpace(string(gotDetach)))
	}
	gotMaint, err := runGit(root, "config", "--get", "maintenance.auto")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(gotMaint)) != "false" {
		t.Fatalf("maintenance.auto via hermetic runGit = %q, want false", strings.TrimSpace(string(gotMaint)))
	}
}

// TestMutationPathGuardsStressNoTempDirResidue runs the exact path-guard
// matrix several times in-process.
func TestMutationPathGuardsStressNoTempDirResidue(t *testing.T) {
	const iterations = 5
	if testing.Short() {
		t.Skip("stress path under -short")
	}
	for i := 0; i < iterations; i++ {
		runMutationPathGuardMatrix(t)
	}
}

func runMutationPathGuardMatrix(t *testing.T) {
	t.Helper()
	dir, _ := verificationRepo(t)
	outsideDir := t.TempDir()
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	writeFile(t, outsideFile, "outside\n")
	gitMetadataProbe := filepath.Join(dir, ".git", "hooks", "fac122-probe")
	writeFile(t, gitMetadataProbe, "metadata\n")

	trackedLink := filepath.Join(dir, "tracked-link")
	if err := os.Symlink(outsideFile, trackedLink); err != nil {
		t.Fatal(err)
	}

	gitParentLink := filepath.Join(dir, "git-parent")
	if err := os.Symlink(".git", gitParentLink); err != nil {
		t.Fatal(err)
	}
	outsideParent := t.TempDir()
	outsideVictim := filepath.Join(outsideParent, "victim.txt")
	writeFile(t, outsideVictim, "outside-parent\n")
	outsideParentLink := filepath.Join(dir, "outside-parent")
	if err := os.Symlink(outsideParent, outsideParentLink); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "tracked-link", "git-parent", "outside-parent")
	git(t, dir, "commit", "-q", "-m", "add mutation guard links")
	candidate := gitOutput(t, dir, "rev-parse", "HEAD")
	before := snapshotWorktree(t, dir)

	cases := []struct {
		target   string
		expected string
	}{
		{target: outsideFile, expected: "relative path"},
		{target: "../outside.txt", expected: "escapes candidate"},
		{target: "nested/../../outside.txt", expected: "escapes candidate"},
		{target: "tracked-link", expected: "Lstat regular file"},
		{target: "git-parent/hooks/fac122-probe", expected: "git metadata"},
		{target: ".git/hooks/fac122-probe", expected: "may not enter .git"},
		{target: "outside-parent/victim.txt", expected: "resolves outside candidate root"},
	}
	for _, tt := range cases {
		_, err := NewVerifierArgs([]string{"true"}).RunMutationCheckForCandidate(context.Background(), dir, MutationRequest{
			CandidateSHA:      candidate,
			EnvironmentPolicy: EnvironmentPolicyInherited,
			TargetFile:        tt.target,
			OriginalCode:      "outside\n",
			MutantCode:        "clobbered\n",
			Timeout:           time.Second,
		})
		if err == nil || !strings.Contains(err.Error(), tt.expected) {
			t.Fatalf("target %q: want %q, got %v", tt.target, tt.expected, err)
		}
		assertFile(t, outsideFile, "outside\n")
		assertFile(t, outsideVictim, "outside-parent\n")
		assertFile(t, gitMetadataProbe, "metadata\n")
		assertWorktreeSnapshot(t, dir, before)
	}
	assertClean(t, dir)
}
