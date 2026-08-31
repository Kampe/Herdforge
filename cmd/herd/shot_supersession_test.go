package main

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/committime"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

func validShotSupersessionFacts() shotSupersessionFacts {
	return shotSupersessionFacts{
		ReportedRef: "FAC-662", ReportedLease: 1, ReplacementSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		ProviderType: "kaneo", ConfiguredProviderType: "kaneo", ProjectID: "project", ConfiguredProjectID: "project",
		TaskRef: "FAC-662", TaskID: "task-id", ProviderTaskRef: "FAC-662", ProviderTaskID: "task-id", ProviderTaskProjectID: "project", ProviderStatus: "in-progress",
		ReceiptVerified: true, CanonicalReceiptMatches: true, Role: "worker", LeaseGeneration: 1, LeaseLive: true,
		LeaseTaskRef: "FAC-662", SessionID: "worker-session", LaunchSession: "worker-session", BuilderSession: "task-fac-662-sol",
		Model: "gpt-test", LaunchModel: "gpt-test",
		Family: "openai", LaunchFamily: "openai", Branch: "herd/fac-662", GitBranch: "herd/fac-662",
		Worktree: "./.herd/worktrees/fac-662", RegisteredWorktree: "./.herd/worktrees/fac-662",
		BaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", GitBaseSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		GitHeadSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Clean: true, ReplacementReachable: true, LiveLaunch: true,
	}
}

func TestShotCandidateSupersessionEncodingHasOneLifecycleOwner(t *testing.T) {
	_, ownerErr := lifecycle.EncodeCandidateSupersessionEvidence(make(chan struct{}))
	if !errors.Is(ownerErr, lifecycle.ErrCandidateSupersessionEncoding) {
		t.Fatalf("lifecycle encoder did not expose its shared error owner: %v", ownerErr)
	}

	source, err := os.ReadFile("shot_supersession.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), "lifecycle.EncodeCandidateSupersessionEvidence(facts)") {
		t.Fatal("shot supersession does not use the shared lifecycle evidence encoder")
	}

	file, err := parser.ParseFile(token.NewFileSet(), "shot_supersession.go", source, 0)
	if err != nil {
		t.Fatal(err)
	}
	ast.Inspect(file, func(node ast.Node) bool {
		literal, ok := node.(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			return true
		}
		value, unquoteErr := strconv.Unquote(literal.Value)
		if unquoteErr != nil {
			t.Fatalf("unquote %s: %v", literal.Value, unquoteErr)
		}
		value = strings.ToLower(value)
		if strings.Contains(value, "supersession") &&
			(strings.Contains(value, "encode") || strings.Contains(value, "marshal") || strings.Contains(value, "serializ")) {
			t.Errorf("cmd/herd independently owns supersession encoding message %q", value)
		}
		return true
	})
}

func TestValidateShotSupersessionFactsRejectsEveryAuthorityAndGitMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*shotSupersessionFacts)
	}{
		{"reported ref", func(f *shotSupersessionFacts) { f.ReportedRef = "FAC-WRONG" }},
		{"reported lease", func(f *shotSupersessionFacts) { f.ReportedLease = 2 }},
		{"signed context project", func(f *shotSupersessionFacts) { f.ProjectID = "wrong" }},
		{"signed context task ref", func(f *shotSupersessionFacts) { f.TaskRef = "FAC-WRONG" }},
		{"signed context task id", func(f *shotSupersessionFacts) { f.TaskID = "wrong" }},
		{"configured project", func(f *shotSupersessionFacts) { f.ConfiguredProjectID = "wrong" }},
		{"provider", func(f *shotSupersessionFacts) { f.ConfiguredProviderType = "wrong" }},
		{"task ref", func(f *shotSupersessionFacts) { f.ProviderTaskRef = "FAC-WRONG" }},
		{"task id", func(f *shotSupersessionFacts) { f.ProviderTaskID = "wrong" }},
		{"task project", func(f *shotSupersessionFacts) { f.ProviderTaskProjectID = "wrong" }},
		{"provider unknown", func(f *shotSupersessionFacts) { f.ProviderStatus = "unknown" }},
		{"signature", func(f *shotSupersessionFacts) { f.ReceiptVerified = false }},
		{"canonical receipt", func(f *shotSupersessionFacts) { f.CanonicalReceiptMatches = false }},
		{"role", func(f *shotSupersessionFacts) { f.Role = "reviewer" }},
		{"lease token", func(f *shotSupersessionFacts) { f.LeaseLive = false }},
		{"lease generation", func(f *shotSupersessionFacts) { f.LeaseGeneration = 2 }},
		{"session", func(f *shotSupersessionFacts) { f.LaunchSession = "other" }},
		{"builder session", func(f *shotSupersessionFacts) { f.BuilderSession = "" }},
		{"model", func(f *shotSupersessionFacts) { f.LaunchModel = "other" }},
		{"family", func(f *shotSupersessionFacts) { f.LaunchFamily = "other" }},
		{"branch", func(f *shotSupersessionFacts) { f.GitBranch = "herd/wrong" }},
		{"worktree", func(f *shotSupersessionFacts) { f.RegisteredWorktree = "./.herd/worktrees/wrong" }},
		{"base", func(f *shotSupersessionFacts) { f.GitBaseSHA = "cccccccccccccccccccccccccccccccccccccccc" }},
		{"head", func(f *shotSupersessionFacts) { f.GitHeadSHA = "cccccccccccccccccccccccccccccccccccccccc" }},
		{"dirty", func(f *shotSupersessionFacts) { f.Clean = false }},
		{"unreachable", func(f *shotSupersessionFacts) { f.ReplacementReachable = false }},
		{"no live launch", func(f *shotSupersessionFacts) { f.LiveLaunch = false }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			facts := validShotSupersessionFacts()
			tc.mutate(&facts)
			if err := validateShotSupersessionFacts(facts); err == nil {
				t.Fatal("invalid supersession facts were accepted")
			}
		})
	}
}

func TestValidateShotSupersessionFactsAcceptsExactAuthority(t *testing.T) {
	if err := validateShotSupersessionFacts(validShotSupersessionFacts()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateShotSupersessionFactsAcceptsCoordinatorIssuedRecoveryReceiptForClosedFAC631Worker(t *testing.T) {
	facts := validShotSupersessionFacts()
	facts.ReportedRef, facts.TaskRef, facts.ProviderTaskRef = "FAC-631", "FAC-631", "FAC-631"
	facts.Role = "recovery"
	facts.AuthorityScope = "candidate-supersession"
	facts.LeaseTaskRef = "FAC-631:recovery"
	facts.ReplacementSHA = "7767a0b613d8449f36f4722cbd43356664d8f1bf"
	facts.AuthorizedCandidateSHA = facts.ReplacementSHA
	facts.GitHeadSHA = facts.ReplacementSHA
	facts.Branch, facts.GitBranch = "herd/fac-631", "herd/fac-631"
	facts.Worktree, facts.RegisteredWorktree = "./.herd/worktrees/fac-631", "./.herd/worktrees/fac-631"
	facts.SessionID, facts.LaunchSession = "recovery-session", "recovery-session"
	facts.BuilderSession = "task-fac-631-sol"
	facts.LiveLaunch = false
	if err := validateShotSupersessionFacts(facts); err != nil {
		t.Fatalf("coordinator-issued recovery receipt rejected for closed FAC-631 worker: %v", err)
	}

	for _, tc := range []struct {
		name   string
		mutate func(*shotSupersessionFacts)
	}{
		{"wrong recovery lease", func(f *shotSupersessionFacts) { f.LeaseTaskRef = "FAC-631" }},
		{"wrong authorized candidate", func(f *shotSupersessionFacts) { f.AuthorizedCandidateSHA = strings.Repeat("c", 40) }},
		{"generic recovery scope", func(f *shotSupersessionFacts) { f.AuthorityScope = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invalid := facts
			tc.mutate(&invalid)
			if err := validateShotSupersessionFacts(invalid); err == nil {
				t.Fatal("invalid coordinator recovery receipt was accepted")
			}
		})
	}
}

func TestCandidateSupersessionRecoveryAuthorityCannotDowngradeToGenericSentinel(t *testing.T) {
	facts := validShotSupersessionFacts()
	facts.Role = "recovery"
	facts.LeaseTaskRef = "FAC-662:recovery"
	facts.AuthorizedCandidateSHA = facts.ReplacementSHA
	facts.LiveLaunch = false
	if err := validateShotSupersessionFacts(facts); err == nil {
		t.Fatal("generic recovery-sentinel receipt was accepted as candidate-supersession authority")
	}
}

func TestExactShotLaunchReceiptKeepsClosedFAC631BuilderProvenanceWithoutInventingLiveState(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".herd", "launch-receipts.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	sink := &launch.JSONLSink{Path: path}
	candidate := "7767a0b613d8449f36f4722cbd43356664d8f1bf"
	commitTime := time.Unix(1_700_000_100, 0).UTC()
	if err := sink.Write(launch.Receipt{
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(), TaskRef: "FAC-631", Role: launch.WorkerRole,
		Provider: "codex", Model: "gpt-5.6-sol", BuilderFamily: "openai", Accepted: true,
		Name: "task-fac-631-sol", Branch: "herd/fac-631", CWD: "/repo/.herd/worktrees/fac-631", CandidateSHA: candidate,
	}); err != nil {
		t.Fatal(err)
	}
	// A newer relaunch after the candidate was authored must not take credit.
	if err := sink.Write(launch.Receipt{
		CreatedAt: commitTime.Add(time.Second), TaskRef: "FAC-631", Role: launch.WorkerRole,
		Provider: "grok", Model: "grok-later", BuilderFamily: "xai", Accepted: true,
		Name: "later-worker", Branch: "herd/fac-631", CWD: "/repo/.herd/worktrees/fac-631",
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := exactShotLaunchReceipt(root, "FAC-631", "herd/fac-631", "/repo/.herd/worktrees/fac-631", candidate, commitTime)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Name != "task-fac-631-sol" || receipt.Model != "gpt-5.6-sol" || receipt.BuilderFamily != "openai" {
		t.Fatalf("historical exact builder provenance = %+v", receipt)
	}
}

func TestExactShotLaunchReceiptUsesCommitterTimeForAmendedFAC631Candidate(t *testing.T) {
	root := t.TempDir()
	gitStamped(t, root, nil, "init", "-q", "-b", "herd/fac-631")
	authorTime := "2026-08-27T20:00:00+00:00"
	commitTime := "2026-08-27T22:00:00+00:00"
	gitStamped(t, root, []string{"GIT_AUTHOR_DATE=" + authorTime, "GIT_COMMITTER_DATE=" + authorTime},
		"commit", "-q", "--allow-empty", "-m", "original")
	gitStamped(t, root, []string{"GIT_COMMITTER_DATE=" + commitTime},
		"commit", "-q", "--amend", "--no-edit", "--allow-empty")
	candidate := gitStamped(t, root, nil, "rev-parse", "HEAD")
	created := committime.Of(root, candidate)
	if created.IsZero() {
		t.Fatal("canonical committer time was not resolved")
	}
	launchTime := time.Date(2026, 8, 27, 21, 0, 0, 0, time.UTC)
	if !launchTime.After(time.Date(2026, 8, 27, 20, 0, 0, 0, time.UTC)) || launchTime.After(created) {
		t.Fatalf("fixture does not straddle retained author time and committer time: launch=%s commit=%s", launchTime, created)
	}

	path := launch.ReceiptPathFor(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := (&launch.JSONLSink{Path: path}).Write(launch.Receipt{
		CreatedAt: launchTime, TaskRef: "FAC-631", Role: launch.WorkerRole,
		Provider: "codex", Model: "gpt-5.6-sol", BuilderFamily: "openai", Accepted: true,
		Name: "task-fac-631-sol", Branch: "herd/fac-631", CWD: root, CandidateSHA: candidate,
	}); err != nil {
		t.Fatal(err)
	}
	receipt, err := exactShotLaunchReceipt(root, "FAC-631", "herd/fac-631", root, candidate, created)
	if err != nil {
		t.Fatalf("launch after retained author time but before amend committer time was rejected: %v", err)
	}
	if receipt.Name != "task-fac-631-sol" {
		t.Fatalf("amended candidate provenance = %+v", receipt)
	}
}

func TestShotRegisteredWorktreeUsesCanonicalContainmentAndRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(root, "init", "-q")
	run(root, "config", "user.email", "test@example.invalid")
	run(root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(root, "add", "README.md")
	run(root, "commit", "-q", "-m", "root")
	worktree := filepath.Join(root, ".herd", "worktrees", "fac-662")
	if err := os.MkdirAll(filepath.Dir(worktree), 0o755); err != nil {
		t.Fatal(err)
	}
	run(root, "worktree", "add", "-q", "-b", "herd/fac-662", worktree)

	lexicalAlias := filepath.Join(worktree, "..", "fac-662")
	portable, registered, err := shotRegisteredWorktree(context.Background(), root, lexicalAlias)
	if err != nil || portable != "./.herd/worktrees/fac-662" || registered != portable {
		t.Fatalf("canonical registered worktree rejected: portable=%q registered=%q err=%v", portable, registered, err)
	}

	outside := t.TempDir()
	escape := filepath.Join(root, ".herd", "worktrees", "escape")
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	if _, _, err := shotRegisteredWorktree(context.Background(), root, escape); err == nil {
		t.Fatal("symlink escape outside canonical worktree root was accepted")
	}
	lexicalEscape := filepath.Join(root, ".herd", "worktrees", "..", "..", "..", filepath.Base(outside))
	if _, _, err := shotRegisteredWorktree(context.Background(), root, lexicalEscape); err == nil {
		t.Fatal("lexical escape outside canonical worktree root was accepted")
	}
}
