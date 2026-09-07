package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/integration"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// The Git effects, native adapter, compiled admission, transaction persistence,
// and runtime build/install below are real. Only the hosted PR API is a local
// executable fixture; this test never contacts GitHub or a live board.
func TestFAC601NativeCycleResumesAfterProcessDiesFollowingMerge(t *testing.T) {
	if root := os.Getenv("FAC601_CRASH_CHILD_ROOT"); root != "" {
		sha := os.Getenv("FAC601_CRASH_SHA")
		ledger, err := reviewledger.NewReviewLedger(root, filepath.Join(root, ".herd", "review-ledger.jsonl"))
		if err != nil {
			t.Fatal(err)
		}
		tasks := provider.NewMemoryProvider()
		tasks.AddTask(&provider.Task{ID: "fixture-601", Ref: "FAC-601", Title: "integration", Description: "candidate acceptance", Status: provider.StatusInReview, ProjectID: "fixture"})
		a := &drainAdapters{root: root, project: "fixture", tasks: tasks, ledger: ledger, head: drainGitHead(root), patchID: drainPatchID(root)}
		a.run = a.liveDrainIntegration
		_ = a.integrate(context.Background(), drainActionEvidence{SHA: sha}, false)
		t.Fatal("fixture did not kill the integration process after publication")
	}

	a, sha, _ := nativeDrainFixtureFiles(t, map[string]string{
		"go.mod": "module github.com/Kampe/Herdforge\n\ngo 1.25\n",
		"main.go": `package main
import "github.com/Kampe/Herdforge/pkg/provenance"
func main() { println(provenance.BinaryRevision) }
`,
		"pkg/provenance/stamp.go": "package provenance\nvar BinaryRevision string\n",
		"scripts/build-herd.sh": `#!/bin/sh
set -eu
mkdir -p bin
rev=$(git rev-parse HEAD)
go build -buildvcs=true -ldflags "-X github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision=$rev" -o bin/herd .
`,
	})
	base := nativeFixtureGit(t, a.root, "rev-parse", "origin/main")
	remote := nativeFixtureGit(t, a.root, "remote", "get-url", "origin")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git unavailable: %v", err)
	}
	fixture := t.TempDir()
	t.Setenv("FAC601_CRASH_GIT", realGit)
	t.Setenv("FAC601_CRASH_REMOTE", remote)
	t.Setenv("FAC601_CRASH_FIXTURE", fixture)
	t.Setenv("FAC601_CRASH_SHA", sha)
	t.Setenv("FAC601_CRASH_BASE", base)
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "")
	write := func(name, body string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fixture, name), []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	// Hosted identity is fixture metadata. All actual Git reads and writes go
	// to the bare temporary remote. Kill only this test's explicit child.
	write("git", `#!/bin/sh
set -eu
if [ "$1" = -C ] && [ "${3:-}" = config ] && [ "${4:-}" = --get ] && [ "${5:-}" = remote.origin.url ]; then
 printf '%s\n' 'git@fixture.invalid:Kampe/Herdforge.git'
 exit 0
fi
if [ "$1" = remote ] && [ "$2" = get-url ]; then
 printf '%s\n' 'git@fixture.invalid:Kampe/Herdforge.git'
 exit 0
fi
if [ "$1" = push ]; then
 for arg do
  case "$arg" in
   *:refs/heads/main)
    printf '%s\n' merge >> "$FAC601_CRASH_FIXTURE/pushes"
    "$FAC601_CRASH_GIT" "$@"
    if [ "${FAC601_CRASH_KILL:-}" = 1 ]; then
     "$FAC601_CRASH_GIT" update-ref refs/remotes/origin/main "$FAC601_CRASH_BASE"
     printf '%s\n' published > "$FAC601_CRASH_FIXTURE/killed-after-push"
     kill -KILL "$PPID"
    fi
    exit 0
    ;;
  esac
 done
fi
exec "$FAC601_CRASH_GIT" "$@"
`, 0755)
	write("gh", `#!/bin/sh
set -eu
case "$1:$2" in
 pr:create)
  cat >/dev/null
  touch "$FAC601_CRASH_FIXTURE/created"
  printf '%s\n' https://fixture.invalid/Kampe/Herdforge/pull/1
  ;;
 pr:list)
  if [ ! -f "$FAC601_CRASH_FIXTURE/created" ]; then printf '[]\n'; exit 0; fi
  main=$("$FAC601_CRASH_GIT" --git-dir="$FAC601_CRASH_REMOTE" rev-parse refs/heads/main)
  if [ "$main" = "$FAC601_CRASH_SHA" ]; then
   cat "$FAC601_CRASH_FIXTURE/merged.json"
  else cat "$FAC601_CRASH_FIXTURE/open.json"; fi
  ;;
 pr:view) cat "$FAC601_CRASH_FIXTURE/view.json" ;;
 *) exit 73 ;;
esac
`, 0755)
	cross := false
	pr := harvest.IntegrationPR{Number: 1, State: "OPEN", URL: "https://fixture.invalid/Kampe/Herdforge/pull/1", Head: sha, Branch: harvest.IntegrationNamespace + "fac-601-" + sha[:12], Base: "main", CrossRepo: &cross}
	raw, err := json.Marshal([]harvest.IntegrationPR{pr})
	if err != nil {
		t.Fatal(err)
	}
	write("open.json", string(raw), 0600)
	pr.State = "MERGED"
	pr.MergeCommit.OID = sha
	pr.MergedAt = "2026-09-07T12:00:00Z"
	raw, err = json.Marshal([]harvest.IntegrationPR{pr})
	if err != nil {
		t.Fatal(err)
	}
	write("merged.json", string(raw), 0600)
	write("view.json", `{"headRefOid":"`+sha+`","mergeable":"CLEAN","statusCheckRollup":[{"name":"Build, Preflight & Test Suite","status":"COMPLETED","conclusion":"SUCCESS"}]}`, 0600)
	t.Setenv("PATH", fixture+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, step := range []integration.Step{integration.StepPass, integration.StepHarvest, integration.StepIntegrationPR} {
		if err := a.integrate(context.Background(), drainActionEvidence{SHA: sha}, false); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	child := exec.CommandContext(ctx, exe, "-test.run=^TestFAC601NativeCycleResumesAfterProcessDiesFollowingMerge$", "-test.count=1")
	child.Env = append(os.Environ(), "FAC601_CRASH_CHILD_ROOT="+a.root, "FAC601_CRASH_KILL=1")
	out, childErr := child.CombinedOutput()
	if ctx.Err() != nil || childErr == nil {
		t.Fatalf("child did not die at publication: %v %s", childErr, out)
	}
	if _, err := os.Stat(filepath.Join(fixture, "killed-after-push")); err != nil {
		t.Fatalf("child failed before publication: %v\n%s", childErr, out)
	}
	tx, err := integration.Load(a.root, sha)
	if err != nil || tx.Pending == nil || tx.Pending.Step != integration.StepMerge || tx.Completed(integration.StepMerge) {
		t.Fatalf("merge interruption did not retain intent: %+v %v", tx, err)
	}
	for _, step := range []integration.Step{integration.StepMerge, integration.StepRuntimeBind, integration.StepPatchProof} {
		if err := a.integrate(context.Background(), drainActionEvidence{SHA: sha}, false); err != nil {
			t.Fatalf("resume %s: %v", step, err)
		}
		tx, err = integration.Load(a.root, sha)
		if err != nil || tx.Done[len(tx.Done)-1].Step != step || tx.Completed(integration.StepCleanup) {
			t.Fatalf("resume escaped step %s: %+v %v", step, tx, err)
		}
	}
	pushes, err := os.ReadFile(filepath.Join(fixture, "pushes"))
	if err != nil || strings.Count(string(pushes), "merge\n") != 1 {
		t.Fatalf("merge was republished: %q %v", pushes, err)
	}
	if err := a.integrate(context.Background(), drainActionEvidence{SHA: sha}, false); err == nil {
		t.Fatal("cleanup admitted without fixture owner/board authority")
	}
	tx, err = integration.Load(a.root, sha)
	if err != nil || tx.Completed(integration.StepCleanup) {
		t.Fatalf("refused cleanup recorded success: %+v %v", tx, err)
	}
	// Prove the integration tree's distinct native ownership with a real
	// claim database present, and prove that ownership never beats a live lease.
	var batch harvest.IntegrationBatch
	if err := json.Unmarshal([]byte(tx.Done[0].Evidence), &batch); err != nil {
		t.Fatal(err)
	}
	n := &nativeIntegrationSteps{root: a.root, project: a.project, tasks: a.tasks, ledger: a.ledger}
	plan, err := n.plan(batch, *tx, integration.Intent{Step: integration.StepCleanup})
	if err != nil {
		t.Fatal(err)
	}
	store, err := claim.NewSQLiteLeaseStore(filepath.Join(a.root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	integrationPath := filepath.Join(a.root, plan.Worktree)
	if err := worktree.RefuseRemovalWithLiveLease(context.Background(), a.root, integrationPath); err == nil {
		t.Fatal("fixture did not reproduce missing dispatch history")
	}
	if err := n.integrationWorktreeOwned(context.Background(), batch, plan, *tx); err != nil {
		t.Fatal("native creation evidence did not establish ownership:", err)
	}
	key := claim.LeaseKey{Repo: a.root, Provider: "memory", Project: "fixture", TaskRef: "FAC-601"}
	lease, err := store.Acquire(context.Background(), key, "fixture-live-owner", "worker", integrationPath, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.integrationWorktreeOwned(context.Background(), batch, plan, *tx); err == nil {
		t.Fatal("live claim did not fence native ownership")
	}
	if _, _, err := store.Release(context.Background(), key, "fixture-live-owner", lease.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	// Removing the exact creation record must fence even an otherwise clean,
	// landed, proven and unleased integration worktree.
	var creation integration.Record
	for _, r := range tx.Done {
		if r.Step == integration.StepHarvest {
			creation = r
		}
	}
	intent := integration.Intent{Candidate: sha, ID: creation.OperationID, Step: integration.StepHarvest}
	creationPath := nativeIntegrationStatePath(a.root, intent, "plan")
	if err := os.Rename(creationPath, creationPath+".preserved"); err != nil {
		t.Fatal(err)
	}
	if err := n.integrationWorktreeOwned(context.Background(), batch, plan, *tx); err == nil {
		t.Fatal("missing creation evidence admitted ownership")
	}
	if err := os.Rename(creationPath+".preserved", creationPath); err != nil {
		t.Fatal(err)
	}
	if err := n.removeIntegrationSource(context.Background(), batch, plan, *tx); err != nil {
		t.Fatal("native-owned integration tree could not retire:", err)
	}
	if err := n.removeIntegrationSource(context.Background(), batch, plan, *tx); err != nil {
		t.Fatal("native-owned retirement did not resume:", err)
	}
	if _, err := os.Stat(integrationPath); !os.IsNotExist(err) {
		t.Fatal("integration tree was not removed")
	}
	// Cleanup is deliberately separate: this fixture has no live board signer
	// or owner retirement authority, and must retain its source worktrees.
	if _, err := os.Stat(filepath.Join(a.root, ".herd", "worktrees", "fac-601")); err != nil {
		t.Fatal("source removed without cleanup authority")
	}
}
