package review

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/provider"
)

func gitDrain(t *testing.T, dir string, args ...string) string {
	t.Helper()
	// Hermetic git: never consult host signing/1Password (ambient agent
	// sockets turn fixture commits into host-process coupling).
	cmd := testgit.Command(dir, args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func TestPipelineContract_EmptyIsDeterministicAndUnknownBoardFailsClosed(t *testing.T) {
	d := t.TempDir()
	p := NewPipeline(Drain{RepoRoot: d, LedgerPath: filepath.Join(d, "ledger.jsonl"), Cap: 8, WindDown: true})
	r, err := p.Scan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.WindDown || r.Pressure != "ok" || r.Pending != 0 || r.Harvestable != 0 || r.NeedReview != 0 {
		t.Fatalf("empty report=%+v", r)
	}
	if r.KaneoOK || r.KaneoInReview != -1 || r.KaneoError == "" {
		t.Fatalf("unknown board was not fail-closed: %+v", r)
	}
	var packet map[string]json.RawMessage
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &packet); err != nil {
		t.Fatal(err)
	}
	want := []string{"wind_down", "pending", "harvest_queue", "refactoring_count", "harvestable", "need_review", "review_pass", "harvest_ready", "content_merged_already", "kaneo_in_review", "cap", "pressure", "kaneo_ok", "kaneo_project", "kaneo_error", "park_branches", "park_cha_with_dups", "review_gate_skips_7d", "ledger_pass", "review_artifacts_rejected", "rebase_needed", "stale_behind_max", "harvestable_shas", "need_review_shas", "content_merged_already_shas", "review_pass_shas", "harvest_ready_shas", "rebase_needed_shas"}
	if len(packet) != len(want) {
		t.Fatalf("JSON keys=%d want %d: %v", len(packet), len(want), packet)
	}
	for _, k := range want {
		if _, ok := packet[k]; !ok {
			t.Errorf("missing JSON key %q", k)
		}
	}
}

func TestPipelineContract_ContentMergedIsExcluded(t *testing.T) {
	d := t.TempDir()
	gitDrain(t, d, "init", "-q", "-b", "main")
	gitDrain(t, d, "config", "user.email", "test@herdforge.local")
	gitDrain(t, d, "config", "user.name", "test")
	gitDrain(t, d, "commit", "--allow-empty", "-q", "-m", "base")
	branch := filepath.Join(d, "lane")
	gitDrain(t, d, "worktree", "add", "-q", "-b", "lane", branch)
	if err := os.WriteFile(filepath.Join(branch, "same.txt"), []byte("same patch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, branch, "add", "same.txt")
	gitDrain(t, branch, "commit", "-q", "-m", "lane metadata")
	tip := strings.TrimSpace(gitDrain(t, branch, "rev-parse", "HEAD"))
	// Independently commit the same file contents on main. The metadata and
	// parent differ, so this is a patch-equivalent zombie, not an ancestor.
	if err := os.WriteFile(filepath.Join(d, "same.txt"), []byte("same patch\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, d, "add", "same.txt")
	gitDrain(t, d, "commit", "-q", "-m", "main metadata")
	mainTip := strings.TrimSpace(gitDrain(t, d, "rev-parse", "HEAD"))
	gitDrain(t, d, "update-ref", "refs/remotes/origin/main", mainTip)
	if tip == mainTip {
		t.Fatal("lane and main tips must have distinct SHAs")
	}
	ancestor := testgit.Command(d, "merge-base", "--is-ancestor", tip, "origin/main")
	if err := ancestor.Run(); err == nil {
		t.Fatal("lane tip unexpectedly reached origin/main; zombie test is vacuous")
	}
	p := NewPipeline(Drain{RepoRoot: d, LedgerPath: filepath.Join(d, "ledger.jsonl"), Cap: 8})
	r, err := p.Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: branch, Branch: "lane", Unmerged: []string{tip}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Shas.ContentMerged) != 1 || r.Shas.ContentMerged[0] != tip {
		t.Fatalf("content merged=%v tip=%q", r.Shas.ContentMerged, tip)
	}
	if r.NeedReview != 0 || r.Harvestable != 0 {
		t.Fatalf("zombie entered review: %+v", r)
	}
}

func setupPipelineRepo(t *testing.T) (root, lane string) {
	t.Helper()
	root = t.TempDir()
	gitDrain(t, root, "init", "-q", "-b", "main")
	gitDrain(t, root, "config", "user.email", "test@herdforge.local")
	gitDrain(t, root, "config", "user.name", "test")
	gitDrain(t, root, "commit", "--allow-empty", "-q", "-m", "base")
	base := strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD"))
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", base)
	lane = filepath.Join(root, "lane")
	gitDrain(t, root, "worktree", "add", "-q", "-b", "lane", lane)
	return
}

func addPass(t *testing.T, root, sha, branch string) {
	t.Helper()
	l := OpenLedger(filepath.Join(root, "ledger.jsonl"))
	writeJSONL(t, l.Path, LedgerRow{Event: "record", SHA: sha, Branch: branch, Tier: "R1", BuilderFamily: "anthropic"}, LedgerRow{Event: "verdict", SHA: sha, Reviewer: "reviewer", Verdict: "PASS"})
}

func TestPipelineContract_RequiredStates(t *testing.T) {
	t.Run("stale PASS is rebase-needed", func(t *testing.T) {
		root, lane := setupPipelineRepo(t)
		if err := os.WriteFile(filepath.Join(lane, "lane.txt"), []byte("lane\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", "lane.txt")
		gitDrain(t, lane, "commit", "-q", "-m", "lane")
		tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))
		for i := 0; i < 25; i++ {
			gitDrain(t, root, "commit", "--allow-empty", "-q", "-m", fmt.Sprintf("main-%d", i))
		}
		gitDrain(t, root, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD")))
		addPass(t, root, tip, "lane")
		r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl")}).Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
		if e != nil {
			t.Fatal(e)
		}
		if r.HarvestReady != 0 || r.Harvestable != 1 || len(r.Shas.RebaseNeeded) != 1 || r.Pins[0].Behind <= 20 || r.Pins[0].Lane != "lane" {
			t.Fatalf("stale report=%+v", r)
		}
		if len(r.ActionEvidence) != 1 || r.ActionEvidence[0].BuilderFamily != "anthropic" || r.ActionEvidence[0].Tier != "R1" || !r.ActionEvidence[0].TierRecorded || !r.ActionEvidence[0].RebaseNeeded {
			t.Fatalf("stale action evidence=%+v", r.ActionEvidence)
		}
	})
	t.Run("duplicate SHA is one pin", func(t *testing.T) {
		root, lane := setupPipelineRepo(t)
		if err := os.WriteFile(filepath.Join(lane, "x"), []byte("x"), 0600); err != nil {
			t.Fatal(err)
		}
		gitDrain(t, lane, "add", "x")
		gitDrain(t, lane, "commit", "-q", "-m", "x")
		tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))
		gitDrain(t, root, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD")))
		addPass(t, root, tip, "lane")
		r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl")}).Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}, {WorktreePath: "other", Branch: "other", Unmerged: []string{tip}}})
		if e != nil {
			t.Fatal(e)
		}
		if len(r.Pins) != 1 {
			t.Fatalf("duplicate pins=%+v", r.Pins)
		}
	})
	t.Run("cap pressure from pending", func(t *testing.T) {
		root, _ := setupPipelineRepo(t)
		l := OpenLedger(filepath.Join(root, "ledger.jsonl"))
		writeJSONL(t, l.Path, LedgerRow{Event: "record", SHA: "pending", Reviewer: "r"})
		r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: l.Path, Cap: 1}).Scan(context.Background(), nil)
		if e != nil {
			t.Fatal(e)
		}
		if r.Pressure != "PIN_PRESSURE" {
			t.Fatalf("pressure=%s", r.Pressure)
		}
	})
}

func TestPipelineContract_PendingUsesOrderedEventIndex(t *testing.T) {
	s := LedgerSnapshot{Rows: []LedgerRow{{Timestamp: "2025-01-01T00:00:02Z", Event: "verdict", SHA: "same", Reviewer: "r", Verdict: "PASS"}, {Timestamp: "2025-01-01T00:00:02Z", Event: "record", SHA: "same", Reviewer: "r"}}}
	if got := s.Pending(); len(got) != 1 || got[0].Event != "record" {
		t.Fatalf("same-timestamp record-after-verdict pending=%+v", got)
	}
	s = LedgerSnapshot{Rows: []LedgerRow{{Timestamp: "2025-01-01T00:00:02Z", Event: "record", SHA: "first", Reviewer: "r"}, {Timestamp: "2025-01-01T00:00:02Z", Event: "record", SHA: "second", Reviewer: "r"}}}
	if got := s.Pending(); len(got) != 2 || got[0].SHA != "first" || got[1].SHA != "second" {
		t.Fatalf("equal-timestamp pending order=%+v", got)
	}
}

func TestPipelineContract_SelfCertificationAndArtifactCounts(t *testing.T) {
	root, _ := setupPipelineRepo(t)
	state := filepath.Join(root, "state")
	if err := os.MkdirAll(state, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "artifacts", "rejected"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, "review-gate-skips.log"), []byte(time.Now().UTC().Format("2006-01-02")+" skip\n2000-01-01 old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "artifacts", "rejected", "x.md"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	l := OpenLedger(filepath.Join(root, "ledger.jsonl"))
	writeJSONL(t, l.Path, LedgerRow{Event: "verdict", SHA: "a", Reviewer: "r", Verdict: "PASS"}, LedgerRow{Event: "verdict", SHA: "a", Reviewer: "r", Verdict: "PASS"})
	r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: l.Path, StateDir: state, ArtifactDir: filepath.Join(root, "artifacts")}).Scan(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.Skips7d != 1 || r.Rejected != 1 || r.LedgerPass != 2 {
		t.Fatalf("self-cert counts=%+v", r)
	}
}

func TestPipelineContract_UnknownEvidenceFailsClosed(t *testing.T) {
	root, _ := setupPipelineRepo(t)
	r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl")}).Scan(context.Background(), []harvest.UnmergedWork{{Branch: "lane", Unmerged: []string{"not-a-sha"}}})
	if e == nil || r != nil {
		t.Fatalf("unknown git evidence report=%+v err=%v", r, e)
	}
}

func TestPipelineContract_ConflictEvidenceIsRebaseNeeded(t *testing.T) {
	root, lane := setupPipelineRepo(t)
	if err := os.WriteFile(filepath.Join(lane, "same.txt"), []byte("lane\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, lane, "add", "same.txt")
	gitDrain(t, lane, "commit", "-q", "-m", "lane conflict")
	tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))
	if err := os.WriteFile(filepath.Join(root, "same.txt"), []byte("main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, root, "add", "same.txt")
	gitDrain(t, root, "commit", "-q", "-m", "main conflict")
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD")))
	addPass(t, root, tip, "lane")
	r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl")}).Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Pins) != 1 || r.Pins[0].Conflict != ConflictRebase {
		t.Fatalf("conflict freshness=%+v", r.Pins)
	}
	if len(r.Shas.RebaseNeeded) != 1 {
		t.Fatalf("rebase-needed=%v", r.Shas.RebaseNeeded)
	}
}

func TestPipelineContract_JSONTypesAndOperationalState(t *testing.T) {
	root, _ := setupPipelineRepo(t)
	gitDrain(t, root, "branch", "park/foo")
	gitDrain(t, root, "branch", "parked/foo")
	tree := strings.TrimSpace(gitDrain(t, root, "write-tree"))
	parkOne := strings.TrimSpace(gitDrain(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "FAC-1 park"))
	parkTwo := strings.TrimSpace(gitDrain(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "FAC-1 parked"))
	parkThree := strings.TrimSpace(gitDrain(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "FAC-1 extra"))
	parkFour := strings.TrimSpace(gitDrain(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "FAC-2 park"))
	parkFive := strings.TrimSpace(gitDrain(t, root, "commit-tree", tree, "-p", "HEAD", "-m", "FAC-2 parked"))
	gitDrain(t, root, "update-ref", "refs/heads/park/foo", parkOne)
	gitDrain(t, root, "update-ref", "refs/heads/parked/foo", parkTwo)
	gitDrain(t, root, "update-ref", "refs/heads/park/bar", parkThree)
	gitDrain(t, root, "update-ref", "refs/heads/park/baz", parkFour)
	gitDrain(t, root, "update-ref", "refs/heads/parked/baz", parkFive)
	gitDrain(t, root, "update-ref", "refs/remotes/origin/park/foo", strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD")))
	t.Setenv("HERD_WIND_DOWN", "1")
	r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl")}).Scan(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if !r.WindDown || r.ParkBranches != 5 || r.ParkCHAWithDups != 2 {
		t.Fatalf("state=%+v", r)
	}
	raw, _ := json.Marshal(r)
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	var parks int
	if err := json.Unmarshal(m["park_branches"], &parks); err != nil {
		t.Fatalf("park_branches must be numeric: %v", err)
	}
}

func TestPipelineContract_BoardGitRows(t *testing.T) {
	root, _ := setupPipelineRepo(t)
	gitDrain(t, root, "branch", "task/FAC-65")
	gitDrain(t, root, "branch", "task/FAC-650")
	gitDrain(t, root, "branch", "park/FAC-650")
	gitDrain(t, root, "branch", "task/spark/FAC-65")
	board := provider.NewMemoryProvider()
	board.AddTask(&provider.Task{ID: "1", Ref: "FAC-65", Title: "A long review task", Status: provider.StatusInReview, ProjectID: "project"})
	r, err := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), Provider: board, KaneoProject: "project"}).Scan(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.KaneoOK || len(r.BoardGit) != 1 || r.BoardGit[0].Ref != "FAC-65" || r.BoardGit[0].Tip != "" || r.BoardGit[0].Park {
		t.Fatalf("board×git rows=%+v", r.BoardGit)
	}
	row := boardGitRow(context.Background(), root, "FAC-65", strings.Repeat("界", 60), nil)
	if len([]rune(row.Title)) != 50 {
		t.Fatalf("title rune length=%d", len([]rune(row.Title)))
	}
	negative := boardGitRow(context.Background(), root, "FAC-65", "title", []PinFreshness{{SHA: "sha650", Branch: "task/FAC-650"}})
	if negative.Tip != "" {
		t.Fatalf("ticket boundary false positive=%+v", negative)
	}
	negativePath := boardGitRow(context.Background(), root, "FAC-65", "title", []PinFreshness{{SHA: "sha650", Branch: "task/fac/650"}})
	if negativePath.Tip != "" {
		t.Fatalf("slash ticket boundary false positive=%+v", negativePath)
	}
	positive := boardGitRow(context.Background(), root, "FAC-65", "title", []PinFreshness{{SHA: "sha65", Branch: "task/FAC-65"}})
	if positive.Tip != "sha65" {
		t.Fatalf("ticket boundary positive miss=%+v", positive)
	}
	parkRoot, _ := setupPipelineRepo(t)
	gitDrain(t, parkRoot, "branch", "task/spark/FAC-65")
	spark := boardGitRow(context.Background(), parkRoot, "FAC-65", "title", nil)
	if spark.Park {
		t.Fatalf("spark branch falsely parked=%+v", spark)
	}
	gitDrain(t, parkRoot, "branch", "park/FAC-65")
	parked := boardGitRow(context.Background(), parkRoot, "FAC-65", "title", nil)
	if !parked.Park {
		t.Fatalf("park branch not detected=%+v", parked)
	}
	pathPositive := boardGitRow(context.Background(), root, "FAC-65", "title", []PinFreshness{{SHA: "sha65", Branch: "task/fac/65"}})
	if pathPositive.Tip != "sha65" {
		t.Fatalf("slash ticket positive miss=%+v", pathPositive)
	}
}

func TestPipelineContract_QueueReenqueueAfterConsume(t *testing.T) {
	d := t.TempDir()
	l := OpenLedger(filepath.Join(d, "ledger.jsonl"))
	writeJSONL(t, l.Path, LedgerRow{Event: "record", SHA: "q", Branch: "old"})
	writeJSONL(t, l.QueuePath, LedgerRow{Event: "enqueue", SHA: "q"}, LedgerRow{Event: "consumed", SHA: "q"}, LedgerRow{Event: "enqueue", SHA: "q", Branch: "new"})
	s, e := l.Snapshot()
	if e != nil {
		t.Fatal(e)
	}
	pins := queuePins(s, nil, map[string]bool{})
	if len(pins) != 1 || pins[0].branch != "new" {
		t.Fatalf("re-enqueue state=%+v", pins)
	}
}

func TestPipelineContract_QueuePreservesLaneEvidence(t *testing.T) {
	s := LedgerSnapshot{Rows: []LedgerRow{{Event: "record", SHA: "q", Branch: "feature/q"}}, Queue: []LedgerRow{{Event: "enqueue", SHA: "q", Branch: "feature/q", Lane: "standing-reviewer"}}}
	pins := queuePins(s, nil, nil)
	if len(pins) != 1 || pins[0].lane != "standing-reviewer" {
		t.Fatalf("queue lane=%+v", pins)
	}
}

func TestPipelineContract_QueueStandingFallback(t *testing.T) {
	s := LedgerSnapshot{Rows: []LedgerRow{{Event: "record", SHA: "q", Branch: "standing/chain-indexer"}}, Queue: []LedgerRow{{Event: "enqueue", SHA: "q"}}}
	pins := queuePins(s, nil, nil)
	if len(pins) != 1 || pins[0].lane != "chain-indexer" {
		t.Fatalf("standing fallback lane=%+v", pins)
	}
}

func TestPipelineContract_UnreadableProbesAreUnknownSentinels(t *testing.T) {
	root := t.TempDir()
	stateFile := filepath.Join(root, "state-file")
	artifactFile := filepath.Join(root, "artifact-file")
	if err := os.WriteFile(stateFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(artifactFile, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: filepath.Join(root, "ledger.jsonl"), StateDir: stateFile, ArtifactDir: artifactFile}).Scan(context.Background(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if r.ParkBranches != -1 || r.Skips7d != -1 || r.Rejected != -1 || len(r.Errors) < 3 {
		t.Fatalf("unreadable probe projection=%+v", r)
	}
	raw, _ := json.Marshal(r)
	var m map[string]json.RawMessage
	json.Unmarshal(raw, &m)
	if string(m["park_branches"]) != "-1" || string(m["review_gate_skips_7d"]) != "-1" || string(m["review_artifacts_rejected"]) != "-1" {
		t.Fatalf("unknown sentinels not in JSON: %s", raw)
	}
}

func TestPipelineContract_VetoSupersession(t *testing.T) {
	root, lane := setupPipelineRepo(t)
	if err := os.WriteFile(filepath.Join(lane, "ok"), []byte("ok"), 0600); err != nil {
		t.Fatal(err)
	}
	gitDrain(t, lane, "add", "ok")
	gitDrain(t, lane, "commit", "-q", "-m", "ok")
	tip := strings.TrimSpace(gitDrain(t, lane, "rev-parse", "HEAD"))
	gitDrain(t, root, "update-ref", "refs/remotes/origin/main", strings.TrimSpace(gitDrain(t, root, "rev-parse", "HEAD")))
	l := OpenLedger(filepath.Join(root, "ledger.jsonl"))
	writeJSONL(t, l.Path, LedgerRow{Event: "record", SHA: tip, Branch: "lane", Tier: "R1"}, LedgerRow{Event: "verdict", SHA: tip, Reviewer: "r", Verdict: "FAIL"}, LedgerRow{Event: "verdict", SHA: tip, Reviewer: "r", Verdict: "PASS"})
	r, e := NewPipeline(Drain{RepoRoot: root, LedgerPath: l.Path}).Scan(context.Background(), []harvest.UnmergedWork{{WorktreePath: lane, Branch: "lane", Unmerged: []string{tip}}})
	if e != nil {
		t.Fatal(e)
	}
	if r.NeedReview != 0 || r.Harvestable != 1 {
		t.Fatalf("superseded veto report=%+v", r)
	}
}
