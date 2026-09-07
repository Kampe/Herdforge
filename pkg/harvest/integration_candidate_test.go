package harvest

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/integration"
)

// Native PR/runtime effects are deliberately fake here. Git discovery, exact
// ledger admission, transaction persistence, and post-merge resumption are real.
type candidateStepsFixture struct {
	root                             string
	t                                *testing.T
	checks, observations, executions []integration.Step
}

func (f *candidateStepsFixture) Check(_ context.Context, _ IntegrationBatch, _ integration.Transaction, in integration.Intent) error {
	f.checks = append(f.checks, in.Step)
	return nil
}
func (f *candidateStepsFixture) Observe(_ context.Context, _ IntegrationBatch, _ integration.Transaction, in integration.Intent) (integration.Observation, error) {
	f.observations = append(f.observations, in.Step)
	body, err := os.ReadFile(filepath.Join(f.root, in.ID+".native-effect"))
	if os.IsNotExist(err) {
		return integration.Observation{Intent: in, State: integration.EffectAbsent}, nil
	}
	if err != nil {
		return integration.Observation{}, err
	}
	return integration.Observation{Intent: in, State: integration.EffectApplied, Evidence: string(body)}, nil
}
func (f *candidateStepsFixture) Execute(_ context.Context, batch IntegrationBatch, tx integration.Transaction, in integration.Intent) error {
	f.executions = append(f.executions, in.Step)
	if in.Step == integration.StepMerge {
		gitInHarvest(f.t, f.root, "merge", "--ff-only", batch.Candidate)
		gitInHarvest(f.t, f.root, "push", "-q", "origin", "main")
	}
	if in.Step == integration.StepCleanup {
		if !tx.Completed(integration.StepPatchProof) {
			return errors.New("cleanup before recorded proof")
		}
		gitInHarvest(f.t, f.root, "worktree", "remove", batch.unmerged(f.root).WorktreePath)
		gitInHarvest(f.t, f.root, "branch", "-d", batch.Branch)
	}
	return os.WriteFile(filepath.Join(f.root, in.ID+".native-effect"), []byte("fixture proof for "+string(in.Step)), 0600)
}

func candidateFixture(t *testing.T) (*Integration, *candidateStepsFixture, string, string) {
	t.Helper()
	root, _ := setupRepoWithRemote(t)
	t.Setenv(integration.StoreDirEnv, "")
	wt := createWorktree(t, root, "task/FAC-601-driver")
	writeFileHarvest(t, wt, "feature.txt", "candidate\n")
	sha := addAndCommitHarvest(t, wt, "feat: candidate", "feature.txt")
	ledger := setupLedger(t, root)
	recordPass(t, ledger, sha, "FAC-601")
	in := NewIntegration(NewHarvester(root), nil, nil, ledger, root, withAdmit("FAC-601"))
	return in, &candidateStepsFixture{root: root, t: t}, sha, wt
}

func TestFAC601CandidateResumesAfterMergeDisappearsFromDiscovery(t *testing.T) {
	in, steps, sha, wt := candidateFixture(t)
	ctx := context.Background()
	for _, step := range integration.Order {
		if step == integration.StepRuntimeBind {
			hr, err := in.Harvester.HarvestReadOnly(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(hr.UnmergedWorktrees) != 0 {
				t.Fatalf("fixture still discovers unmerged work: %+v", hr)
			}
			// Losing discovery must not lose the remaining lifecycle.
			in.Harvester = nil
		}
		res, err := in.RunCandidate(ctx, sha, step, steps)
		if err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		if res.Progress == nil || res.Progress.Candidate != sha || res.Progress.Step != step {
			t.Fatalf("wrong progress: %+v", res)
		}
		if len(res.MergedSHAs) != 0 {
			t.Fatal("intermediate progress was reported as a merge outcome")
		}
		if step == integration.StepMerge {
			before := len(steps.executions)
			if _, err := in.RunCandidate(ctx, sha, step, steps); err != nil {
				t.Fatal(err)
			}
			if len(steps.executions) != before {
				t.Fatal("duplicate merge advanced another native effect")
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatal("merge also removed source worktree")
			}
		}
	}
	tx, err := integration.Load(in.RepoRoot, sha)
	if err != nil {
		t.Fatal(err)
	}
	if len(tx.Done) != len(integration.Order) {
		t.Fatalf("incomplete history: %+v", tx)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("cleanup was not reached after proof: %v", err)
	}
	var batch IntegrationBatch
	if err := json.Unmarshal([]byte(tx.Done[0].Evidence), &batch); err != nil {
		t.Fatal(err)
	}
	if filepath.IsAbs(batch.Worktree) || batch.Candidate != sha || batch.Task != "FAC-601" {
		t.Fatalf("bad retained identity: %+v", batch)
	}
}

func TestFAC601CandidateDryRunHasNoOperationalWrites(t *testing.T) {
	in, steps, sha, _ := candidateFixture(t)
	in.DryRun = true
	res, err := in.RunCandidate(context.Background(), sha, integration.StepPass, steps)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.ReviewGatedSHAs) != 1 || !res.ReviewGatedSHAs[0].Eligible {
		t.Fatal("dry run never exercised real review gate")
	}
	if res.Progress != nil || len(steps.executions) != 0 || len(steps.observations) != 0 {
		t.Fatal("dry run performed a native effect/readback")
	}
	if _, err := os.Stat(integration.Path(in.RepoRoot, sha)); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote transaction: %v", err)
	}
}

func TestFAC601CandidateChecksEverySourceAndLiveHead(t *testing.T) {
	for _, mode := range []string{"head-moved", "missing-review", "unreviewed-prefix"} {
		t.Run(mode, func(t *testing.T) {
			in, steps, sha, wt := candidateFixture(t)
			if mode == "head-moved" {
				if _, err := in.RunCandidate(context.Background(), sha, integration.StepPass, steps); err != nil {
					t.Fatal(err)
				}
				writeFileHarvest(t, wt, "next.txt", "not reviewed\n")
				addAndCommitHarvest(t, wt, "feat: drift", "next.txt")
			} else if mode == "missing-review" {
				in.Ledger = nil
			} else {
				// Only the new tip is admitted. The original source loses its
				// independent review: it must not ride along on that PASS.
				old := sha
				writeFileHarvest(t, wt, "next.txt", "second candidate\n")
				sha = addAndCommitHarvest(t, wt, "feat: second candidate", "next.txt")
				if err := os.Remove(filepath.Join(in.RepoRoot, ".herd", "review-ledger.jsonl")); err != nil {
					t.Fatal(err)
				}
				in.Ledger = setupLedger(t, in.RepoRoot)
				recordPass(t, in.Ledger, sha, "FAC-601")
				if old == sha {
					t.Fatal("fixture did not change candidate")
				}
			}
			step := integration.StepPass
			if mode == "head-moved" {
				step = integration.StepHarvest
			}
			if _, err := in.RunCandidate(context.Background(), sha, step, steps); err == nil {
				t.Fatal("invalid candidate reached execution")
			}
			if len(steps.executions) != 0 {
				t.Fatal("refusal executed native operation")
			}
		})
	}
}

func TestFAC601CandidateCannotSkipProofOrUseOtherCandidateHistory(t *testing.T) {
	in, steps, sha, _ := candidateFixture(t)
	if _, err := in.RunCandidate(context.Background(), sha, integration.StepPass, steps); err != nil {
		t.Fatal(err)
	}
	if _, err := in.RunCandidate(context.Background(), sha, integration.StepCleanup, steps); err == nil {
		t.Fatal("cleanup skipped durable proof")
	}
	if _, err := in.RunCandidate(context.Background(), strings.Repeat("f", 40), integration.StepHarvest, steps); err == nil {
		t.Fatal("another candidate resumed this transaction")
	}
	if len(steps.executions) != 0 {
		t.Fatal("wrong identity/order caused native effects")
	}
}

func TestFAC601CandidateDryRunHonorsActivePredecessor(t *testing.T) {
	in, steps, sha, _ := candidateFixture(t)
	if _, err := in.RunCandidate(context.Background(), sha, integration.StepPass, steps); err != nil {
		t.Fatal(err)
	}
	other := createWorktree(t, in.RepoRoot, "task/FAC-601-next")
	writeFileHarvest(t, other, "next.txt", "next candidate\n")
	next := addAndCommitHarvest(t, other, "next candidate", "next.txt")
	recordPass(t, in.Ledger, next, "FAC-601")
	in.DryRun = true
	if _, err := in.RunCandidate(context.Background(), next, integration.StepPass, steps); err == nil || !strings.Contains(err.Error(), "must finish proof and cleanup") {
		t.Fatalf("preview ignored active predecessor: %v", err)
	}
	if _, err := os.Stat(integration.Path(in.RepoRoot, next)); !os.IsNotExist(err) {
		t.Fatal("refused preview created a transaction")
	}
}
