package harvest

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// -- test helpers --

func gitInHarvest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Bypass gpg/ssh signing in test repos — we are not shipping these commits.
	base := []string{"-c", "commit.gpgSign=false", "-c", "gpg.x509.program=false", "-c", "gpg.format=openpgp", "-c", "tag.gpgSign=false", "-c", "user.email=test@herdforge.local", "-c", "user.name=Test Runner"}
	gitArgs := append(base, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeFileHarvest(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func addAndCommitHarvest(t *testing.T, dir, msg string, files ...string) string {
	t.Helper()
	args := append([]string{"add"}, files...)
	gitInHarvest(t, dir, args...)
	gitInHarvest(t, dir, "commit", "-q", "-m", msg)
	return gitInHarvest(t, dir, "rev-parse", "HEAD")
}

// fakeVerifier implements Verifier for testing.
type fakeVerifier struct {
	pass  bool
	msgs  []string
}

func (f *fakeVerifier) Execute(_ context.Context, _ string) (*VerifyResult, error) {
	result := "passed"
	if !f.pass {
		result = "failed"
	}
	f.msgs = append(f.msgs, result)
	return &VerifyResult{Passed: f.pass, Output: result}, nil
}

// fakeDispatcher implements Dispatcher for testing.
type fakeDispatcher struct {
	completeRefs []string
	failOn       []string
}

func (f *fakeDispatcher) BoardComplete(_ context.Context, ref, _ string) error {
	for _, fb := range f.failOn {
		if ref == fb {
			return fmt.Errorf("refused board-complete for %s", ref)
		}
	}
	f.completeRefs = append(f.completeRefs, ref)
	return nil
}

// -- Tests --

func TestIntegrationDryRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitInHarvest(t, root, "init", "-q", "-b", "main")
	gitInHarvest(t, root, "config", "user.email", "t@h.local")
	gitInHarvest(t, root, "config", "user.name", "t")
	gitInHarvest(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	gitInHarvest(t, root, "update-ref", "refs/remotes/origin/main", gitInHarvest(t, root, "rev-parse", "HEAD"))

	wt := filepath.Join(root, "..", "wt-foo")
	absWT, err := filepath.Abs(wt)
	if err != nil {
		t.Fatal(err)
	}
	gitInHarvest(t, root, "worktree", "add", "-q", "-b", "task/FAC-99-foo", absWT)
	writeFileHarvest(t, absWT, "feat.go", "package main")
	sha := addAndCommitHarvest(t, absWT, "feat: FAC-99 initial implementation", "feat.go")

	h := NewHarvester(root)
	fd := &fakeDispatcher{}
	in := NewIntegration(h, nil, fd, root, WithDryRun(true))
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.HarvestResult == nil {
		t.Fatal("expected HarvestResult")
	}
	if len(res.HarvestResult.UnmergedWorktrees) == 0 {
		t.Fatal("expected at least one unmerged worktree")
	}
	if len(res.ReviewGatedSHAs) == 0 {
		t.Fatal("expected review gate outcomes")
	}
	// Dry run: should see SHA in review gate outcome
	found := false
	for _, rg := range res.ReviewGatedSHAs {
		if rg.SHA == sha {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected SHA %s in review gate outcomes, got %+v", sha, res.ReviewGatedSHAs)
	}
}

func TestIntegrationNoUnmerged(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	gitInHarvest(t, root, "init", "-q", "-b", "main")
	gitInHarvest(t, root, "config", "user.email", "t@h.local")
	gitInHarvest(t, root, "config", "user.name", "t")
	gitInHarvest(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	gitInHarvest(t, root, "update-ref", "refs/remotes/origin/main", gitInHarvest(t, root, "rev-parse", "HEAD"))

	h := NewHarvester(root)
	in := NewIntegration(h, nil, &fakeDispatcher{}, root)
	res, err := in.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.HarvestResult.UnmergedWorktrees) != 0 {
		t.Fatalf("expected 0 unmerged, got %d", len(res.HarvestResult.UnmergedWorktrees))
	}
	if len(res.ReviewGatedSHAs) != 0 {
		t.Fatalf("expected 0 review outcomes, got %d", len(res.ReviewGatedSHAs))
	}
}

func TestReviewGateEligibility(t *testing.T) {
	tests := []struct {
		name         string
		commitMsg    string
		wantEligible bool
	}{
		{
			name:         "pass_verdict_is_eligible",
			commitMsg:    "feat: implement foo\n\nVerdict: PASS\nMerge recommendation: YES",
			wantEligible: true,
		},
		{
			name:         "fail_verdict_is_not_eligible",
			commitMsg:    "feat: implement foo\n\nVerdict: FAIL\nMerge recommendation: NO",
			wantEligible: false,
		},
		{
			name:         "blocked_is_not_eligible",
			commitMsg:    "feat: implement foo\n\nStatus: BLOCKED: waiting for API key",
			wantEligible: false,
		},
		{
			name:         "complete_is_eligible",
			commitMsg:    "feat: implement foo\n\nStatus: COMPLETE",
			wantEligible: true,
		},
		{
			name:         "unknown_is_not_eligible",
			commitMsg:    "some random commit message",
			wantEligible: false,
		},
		{
			name:         "needs_review_with_verifier_pass_is_eligible",
			commitMsg:    "feat: implement foo\n\nStatus: NEEDS_REVIEW",
			wantEligible: true,
		},
		{
			name:         "needs_review_with_verifier_fail_is_not_eligible",
			commitMsg:    "feat: implement foo\n\nStatus: NEEDS_REVIEW",
			wantEligible: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			// Each subtest gets its own isolated repo so the harvester doesn't
			// scan worktrees created by sibling subtests.
			root := t.TempDir()
			gitInHarvest(t, root, "init", "-q", "-b", "main")
			gitInHarvest(t, root, "commit", "--allow-empty", "-q", "-m", "base")
			gitInHarvest(t, root, "update-ref", "refs/remotes/origin/main", gitInHarvest(t, root, "rev-parse", "HEAD"))

			wt := filepath.Join(root, "..", "wt-"+tt.name)
			absWT, err := filepath.Abs(wt)
			if err != nil {
				t.Fatal(err)
			}
			gitInHarvest(t, root, "worktree", "add", "-q", "-b", "feature-branch", absWT)
			writeFileHarvest(t, absWT, "test.go", "package main")
			addAndCommitHarvest(t, absWT, tt.commitMsg, "test.go")

			h := NewHarvester(root)
			var v *fakeVerifier
			if strings.Contains(tt.commitMsg, "NEEDS_REVIEW") {
				v = &fakeVerifier{pass: !strings.Contains(tt.name, "fail")}
			}
			in := NewIntegration(h, v, &fakeDispatcher{}, root, WithDryRun(true))
			res, err := in.Run(ctx)
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if len(res.ReviewGatedSHAs) == 0 {
				t.Fatal("expected at least one review gate outcome")
			}
			for _, rg := range res.ReviewGatedSHAs {
				if rg.Eligible != tt.wantEligible {
					t.Errorf("eligible = %v, want %v (class=%v, err=%s)", rg.Eligible, tt.wantEligible, rg.Classification, rg.Err)
				}
			}
		})
	}
}

func TestBranchToRef(t *testing.T) {
	tests := []struct {
		branch string
		want   string
	}{
		{"task/FAC-99-foo", "FAC-99"},
		{"task/KAN-123-implement", "KAN-123"},
		{"main", "main"},
		{"lane", "lane"},
		{"feature/abc", "feature/abc"},
	}
	for _, tt := range tests {
		t.Run(tt.branch, func(t *testing.T) {
			got := branchToRef(tt.branch)
			if got != tt.want {
				t.Errorf("branchToRef(%q) = %q, want %q", tt.branch, got, tt.want)
			}
		})
	}
}
