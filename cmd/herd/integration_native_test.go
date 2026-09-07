package main

import (
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/integration"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func nativeFixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := testgit.Command(dir, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func nativeDrainFixture(t *testing.T) (*drainAdapters, string, string) {
	return nativeDrainFixtureFiles(t, nil)
}
func nativeDrainFixtureFiles(t *testing.T, files map[string]string) (*drainAdapters, string, string) {
	t.Helper()
	t.Setenv(integration.StoreDirEnv, "")
	root, remote := t.TempDir(), t.TempDir()
	nativeFixtureGit(t, remote, "init", "--bare", "-b", "main")
	nativeFixtureGit(t, root, "init", "-b", "main")
	nativeFixtureGit(t, root, "config", "user.name", "fixture")
	nativeFixtureGit(t, root, "config", "user.email", "fixture@example.com")
	if err := os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".herd/\nbin/\nherd\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	nativeFixtureGit(t, root, "add", ".")
	nativeFixtureGit(t, root, "commit", "-m", "base")
	base := nativeFixtureGit(t, root, "rev-parse", "HEAD")
	nativeFixtureGit(t, root, "remote", "add", "origin", remote)
	nativeFixtureGit(t, root, "push", "-u", "origin", "main")
	source := filepath.Join(root, ".herd", "worktrees", "fac-601")
	if err := os.MkdirAll(filepath.Dir(source), 0755); err != nil {
		t.Fatal(err)
	}
	nativeFixtureGit(t, root, "worktree", "add", "-b", "herd/fac-601", source, base)
	if err := os.WriteFile(filepath.Join(source, "feature.txt"), []byte("candidate\n"), 0644); err != nil {
		t.Fatal(err)
	}
	nativeFixtureGit(t, source, "add", "feature.txt")
	nativeFixtureGit(t, source, "commit", "-m", "candidate")
	sha := nativeFixtureGit(t, source, "rev-parse", "HEAD")
	ledger, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	patch, err := drainPatchID(root)(context.Background(), sha)
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Record(reviewledger.RecordOpts{SHA: sha, Branch: "herd/fac-601", Task: "FAC-601", Lease: "fixture-lease", BuilderFamily: "openai", BuilderIdentity: "fixture-builder", ReviewerFamily: "anthropic", Reviewer: "fixture-reviewer", Tier: "R1", Gate: "independent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Verdict(reviewledger.VerdictOpts{SHA: sha, CandidateSHA: sha, Task: "FAC-601", Lease: "fixture-lease", Reviewer: "fixture-reviewer", ReviewerFamily: "anthropic", BuilderFamily: "openai", Verdict: reviewledger.VerdictPASS, PatchURL: patch, VfyDigest: "fixture-executed-test-digest", Artifact: "fixture.md"}); err != nil {
		t.Fatal(err)
	}
	tasks := provider.NewMemoryProvider()
	task := &provider.Task{ID: "fixture-601", Ref: "FAC-601", Title: "integration", Description: "candidate acceptance", Status: provider.StatusInReview, ProjectID: "fixture"}
	tasks.AddTask(task)
	revision := mergeadmit.TaskContentRevision(task.Ref, task.Title, task.Description)
	req := mergeadmit.Request{Ref: task.Ref, TaskID: task.ID, CandidateSHA: sha, BaseSHA: base, ProviderRevision: revision, AcceptanceDigest: mergeadmit.ComputeAcceptanceDigest(task.Ref, task.ID, revision), Lease: "fixture-lease", LeaseGeneration: 1, PatchURL: patch, AuthorFamily: "openai", AuthorIdentity: "fixture-builder", Mode: mergeadmit.ModeMerge}
	// Fixture input represents a prior source-PR admission, not permission to
	// merge the integration PR. Production merge must run its fresh gate.
	if err := writeAdmissionRecord(root, req, mergeadmit.Decision{Admitted: true, CandidateSHA: sha, BaseSHA: base}); err != nil {
		t.Fatal(err)
	}
	a := &drainAdapters{root: root, project: "fixture", tasks: tasks, ledger: ledger, head: drainGitHead(root), patchID: drainPatchID(root)}
	a.run = a.liveDrainIntegration
	return a, sha, source
}

func TestFAC601LiveDrainAdvancesOnlyOneNativeStep(t *testing.T) {
	a, sha, source := nativeDrainFixture(t)
	e := drainActionEvidence{SHA: sha, Branch: "herd/fac-601", BuilderFamily: "openai", Tier: "R1", TierRecorded: true, HarvestReady: true}
	for _, step := range []integration.Step{integration.StepPass, integration.StepHarvest} {
		if err := a.integrate(context.Background(), e, true); err != nil {
			t.Fatalf("preview %s: %v", step, err)
		}
		if err := a.integrate(context.Background(), e, false); err != nil {
			t.Fatalf("execute %s: %v", step, err)
		}
		tx, err := integration.Load(a.root, sha)
		if err != nil {
			t.Fatal(err)
		}
		if tx.Done[len(tx.Done)-1].Step != step || tx.Completed(integration.StepMerge) || tx.Completed(integration.StepCleanup) {
			t.Fatalf("native step escaped order: %+v", tx)
		}
		if _, err := os.Stat(source); err != nil {
			t.Fatal("source removed before proof")
		}
	}
	path := filepath.Join(a.root, ".herd", "integration-worktrees", sha)
	if got := nativeFixtureGit(t, path, "rev-parse", "HEAD"); got != sha {
		t.Fatal("harvest changed candidate identity")
	}
	if got := nativeFixtureGit(t, a.root, "rev-parse", "origin/main"); got == sha {
		t.Fatal("harvest published main")
	}
}

func TestFAC601NativeHarvestResumesItsRetainedPlan(t *testing.T) {
	a, sha, _ := nativeDrainFixture(t)
	e := drainActionEvidence{SHA: sha}
	if err := a.integrate(context.Background(), e, false); err != nil {
		t.Fatal(err)
	}
	tx, err := integration.Load(a.root, sha)
	if err != nil {
		t.Fatal(err)
	}
	var batch harvest.IntegrationBatch
	if err := json.Unmarshal([]byte(tx.Done[0].Evidence), &batch); err != nil {
		t.Fatal(err)
	}
	n := &nativeIntegrationSteps{root: a.root, project: a.project, tasks: a.tasks, ledger: a.ledger}
	i := integration.Intent{Candidate: sha, ID: strings.Repeat("e", 32), Step: integration.StepHarvest}
	p, err := n.plan(batch, *tx, i)
	if err != nil {
		t.Fatal(err)
	}
	if err := retainNativeIntegrationState(a.root, i, "plan", p); err != nil {
		t.Fatal(err)
	}
	if _, err := n.manager().CreateExactWorktree(context.Background(), p.Branch, filepath.Join(a.root, p.Worktree), sha); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(admissionRecordPath(a.root, batch.Task)); err != nil {
		t.Fatal(err)
	}
	o, err := n.Observe(context.Background(), batch, *tx, i)
	if err != nil || o.State != integration.EffectApplied || o.Evidence == "" {
		t.Fatalf("lost retained operation plan: %+v %v", o, err)
	}
	changed := p
	changed.Request.CandidateSHA = strings.Repeat("b", 40)
	if err := retainNativeIntegrationState(a.root, i, "plan", changed); err == nil {
		t.Fatal("native operation evidence overwritten")
	}
}

func TestFAC601PendingCyclePrecedesOtherDiscoveredCandidates(t *testing.T) {
	a, sha, _ := nativeDrainFixture(t)
	if err := a.integrate(context.Background(), drainActionEvidence{SHA: sha}, false); err != nil {
		t.Fatal(err)
	}
	other := strings.Repeat("a", 40)
	out, err := a.integrationEvidence([]drainActionEvidence{{SHA: other, HarvestReady: true}})
	if err != nil || len(out) != 2 || out[0].SHA != sha || !out[0].IntegrationPending || out[0].HarvestReady || out[1].HarvestReady {
		t.Fatalf("pending candidate was lost or presented as new harvest authority: %+v %v", out, err)
	}
	if err := os.WriteFile(filepath.Join(integration.StoreDir(a.root), "active.json"), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.integrationEvidence(nil); err == nil {
		t.Fatal("corrupt active cycle treated as empty discovery")
	}
}

func TestFAC601ProductionHooksBoundOneIntegrationStep(t *testing.T) {
	calls := 0
	hooks := drainActionHooks{singleIntegrationStep: true,
		launchReview: func(context.Context, drainActionEvidence) error { return nil },
		dryRun:       func(context.Context, drainActionEvidence) error { return nil },
		harvest:      func(context.Context, drainActionEvidence) error { calls++; return nil },
	}
	evidence := []drainActionEvidence{
		{SHA: strings.Repeat("a", 40), HarvestReady: true, BuilderFamily: "openai", Tier: "R1", TierRecorded: true},
		{SHA: strings.Repeat("b", 40), HarvestReady: true, BuilderFamily: "openai", Tier: "R1", TierRecorded: true},
	}
	var out strings.Builder
	result := executeDrainActions(context.Background(), drainTestReport(), evidence, 0, 10, 0, "R1", &out, hooks)
	if calls != 1 || result.IntegrationSteps != 1 || result.Harvests != 0 || result.Failed {
		t.Fatalf("production beat applied multiple steps: calls=%d %+v %s", calls, result, out.String())
	}
}

// A reviewed revision is explicit input. Current live card state must not fill
// this field merely because the rest of the candidate happens to be admissible.
func TestFAC601MergeAdmitCLIRequiresRetainedProviderRevision(t *testing.T) {
	a, _, _ := nativeDrainFixture(t)
	saved, err := readAdmissionRecord(a.root, "FAC-601")
	if err != nil {
		t.Fatal(err)
	}
	req := saved.Request
	gate := &mergeadmit.Gate{RepoDir: a.root, Ledger: a.ledger,
		Policy: preflight.MergePolicy{Protected: true, RequiredChecks: []string{"fixture-build"}, RequireDifferentFamilyReview: true, RequirePullRequestReviews: true},
		Live: mergeadmit.LiveState{
			OriginMain: mergeadmit.StaticProbe(req.BaseSHA), CandidateHead: mergeadmit.StaticProbe(req.CandidateSHA),
			Mergeable: mergeadmit.StaticProbe("CLEAN"), TaskRevision: mergeadmit.StaticProbe(req.ProviderRevision),
			Checks: func() (map[string]string, error) { return map[string]string{"fixture-build": "success"}, nil },
		},
	}
	args := []string{"--ref", req.Ref, "--task-id", req.TaskID, "--candidate", req.CandidateSHA, "--base", req.BaseSHA,
		"--lease", req.Lease, "--lease-generation", "1", "--patch-id", req.PatchURL, "--acceptance-digest", req.AcceptanceDigest,
		"--author-family", req.AuthorFamily, "--author-identity", req.AuthorIdentity, "--mode", string(req.Mode)}
	for _, explicit := range []bool{false, true} {
		fs := flag.NewFlagSet("merge-admit", flag.ContinueOnError)
		f := registerMergeAdmitFlags(fs)
		argv := append([]string(nil), args...)
		if explicit {
			argv = append(argv, "--provider-revision", req.ProviderRevision)
		}
		if err := fs.Parse(argv); err != nil {
			t.Fatal(err)
		}
		d, err := gate.Admit(f.request())
		if explicit {
			if err != nil || d == nil || !d.Admitted {
				t.Fatalf("explicit authentic revision was lost: %+v %v", d, err)
			}
		} else if d == nil || d.Admitted || d.Code != mergeadmit.CodeMissingField {
			t.Fatalf("missing review-time revision did not fail closed: %+v %v", d, err)
		}
	}
}

func TestFAC601RuntimePendingAfterPublishedMergeWithStaleCache(t *testing.T) {
	a, sha, source := nativeDrainFixture(t)
	n := &nativeIntegrationSteps{root: a.root}
	base := nativeFixtureGit(t, a.root, "rev-parse", "origin/main")
	// Real publication, then simulate losing the local tracking update.
	nativeFixtureGit(t, source, "push", "origin", sha+":refs/heads/main")
	nativeFixtureGit(t, a.root, "update-ref", "refs/remotes/origin/main", base)
	pending, err := n.runtimeNeedsFetch(context.Background(), sha)
	if err != nil || !pending {
		t.Fatalf("published merge could not resume: %t %v", pending, err)
	}
	if got := nativeFixtureGit(t, a.root, "rev-parse", "origin/main"); got != base {
		t.Fatal("observation moved the cache")
	}
	nativeFixtureGit(t, source, "fetch", "--no-tags", "origin", "main")
	pending, err = n.runtimeNeedsFetch(context.Background(), sha)
	if err != nil || pending {
		t.Fatalf("refreshed publication remains pending: %t %v", pending, err)
	}
	nativeFixtureGit(t, a.root, "remote", "set-url", "origin", filepath.Join(t.TempDir(), "absent-remote"))
	if _, err := n.runtimeNeedsFetch(context.Background(), sha); err == nil {
		t.Fatal("unknown remote became pending or applied")
	}
}

func TestFAC601ExactSourceCleanupPreservesDirtyAndMovedBranches(t *testing.T) {
	a, sha, source := nativeDrainFixture(t)
	n := &nativeIntegrationSteps{root: a.root}
	rel := ".herd/worktrees/fac-601"
	branch := "herd/fac-601"
	if err := os.WriteFile(filepath.Join(source, "retained-untracked.txt"), []byte("operator evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := n.sourcePresent(context.Background(), rel, branch, sha); err == nil {
		t.Fatal("dirty source admitted for removal")
	}
	if err := n.removeExactSource(context.Background(), rel, branch, sha); err == nil {
		t.Fatal("dirty source was removed")
	}
	if _, err := os.Stat(filepath.Join(source, "retained-untracked.txt")); err != nil {
		t.Fatal("dirty evidence lost")
	}
	if err := os.Remove(filepath.Join(source, "retained-untracked.txt")); err != nil {
		t.Fatal(err)
	}
	nativeFixtureGit(t, source, "commit", "--allow-empty", "-m", "later work")
	if err := n.removeExactSource(context.Background(), rel, branch, sha); err == nil {
		t.Fatal("advanced source was removed")
	}
	if _, err := os.Stat(source); err != nil {
		t.Fatal("advanced worktree lost")
	}
}

func TestFAC601ExactSourceCleanupResumesBetweenTreeAndBranchRemoval(t *testing.T) {
	a, sha, source := nativeDrainFixture(t)
	n := &nativeIntegrationSteps{root: a.root}
	// Simulate a crash after registered tree removal, before branch deletion.
	nativeFixtureGit(t, a.root, "worktree", "remove", source)
	if err := n.removeExactSource(context.Background(), ".herd/worktrees/fac-601", "herd/fac-601", sha); err != nil {
		t.Fatal(err)
	}
	present, err := n.sourcePresent(context.Background(), ".herd/worktrees/fac-601", "herd/fac-601", sha)
	if err != nil || present {
		t.Fatalf("partial cleanup did not settle: %t %v", present, err)
	}
	if err := n.removeExactSource(context.Background(), ".herd/worktrees/fac-601", "herd/fac-601", sha); err != nil {
		t.Fatal("idempotent removal refused:", err)
	}
	if got := nativeFixtureGit(t, a.root, "rev-parse", "main"); got == sha {
		t.Fatal("cleanup moved main")
	}
}
