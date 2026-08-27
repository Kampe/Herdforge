package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
)

const reapTestSHA = "0123456789abcdef0123456789abcdef01234567"

func reapArtifact(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "verdict.md")
	body := "sha: " + reapTestSHA + "\nreviewer: review-fac-585-01234567\nverdict: PASS\n---\nsettled\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func reapAgent(status string, focused bool) herdr.AgentEntry {
	return herdr.AgentEntry{
		Name: "review-fac-585-01234567", Status: status, Focused: &focused,
		TabID: "w4:t1", PaneID: "w4:p1", Workspace: "w4", TerminalID: "term-1",
		Cwd: "/candidate", Session: herdr.AgentSession{Value: "session-1"}, Revision: 7, StateChangeSeq: 9,
	}
}


func emitReapAck(t *testing.T, root, artifactPath string) {
	t.Helper()
	body, err := os.ReadFile(artifactPath)
	if err != nil {
		t.Fatal(err)
	}
	a := reviewingest.Parse(string(body))
	if err := reviewack.Emit(root, reviewack.Ack{
		SHA: a.SHA, Reviewer: a.Reviewer, ArtifactDigest: reviewack.ArtifactDigest(body),
		LaunchIdentity: a.Reviewer,
	}); err != nil {
		t.Fatal(err)
	}
}

func installReapSeams(t *testing.T, agents func() ([]herdr.AgentEntry, error), close func(herdr.AgentEntry) error, head string) {
	t.Helper()
	oldAgents, oldClose, oldHead := transportedReviewerAgents, transportedReviewerClose, transportedReviewerHead
	transportedReviewerAgents = agents
	transportedReviewerClose = close
	transportedReviewerHead = func(string) (string, error) { return head, nil }
	t.Cleanup(func() {
		transportedReviewerAgents, transportedReviewerClose, transportedReviewerHead = oldAgents, oldClose, oldHead
	})
}

func TestReapTransportedReviewerRefusesCandidateHeadMismatch(t *testing.T) {
	artifact := reapArtifact(t, t.TempDir())
	closed := false
	installReapSeams(t, func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{reapAgent("idle", false)}, nil }, func(herdr.AgentEntry) error {
		closed = true
		return nil
	}, strings.Repeat("f", 40))

	r, err := reapTransportedReviewer(t.TempDir(), artifact, false)
	if err == nil || r.Disposition != "blocked" || !strings.Contains(r.Reason, "HEAD mismatch") || closed {
		t.Fatalf("receipt=%+v err=%v closed=%v", r, err, closed)
	}
}

func TestReapTransportedReviewerKeepsFocusedAndWorkingTabs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  string
		focused bool
	}{
		{name: "focused", status: "idle", focused: true},
		{name: "working", status: "working", focused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			artifact := reapArtifact(t, t.TempDir())
			closed := false
			installReapSeams(t, func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{reapAgent(tc.status, tc.focused)}, nil }, func(herdr.AgentEntry) error {
				closed = true
				return nil
			}, reapTestSHA)
			r, err := reapTransportedReviewer(t.TempDir(), artifact, false)
			if err != nil || r.Disposition != "retained" || closed {
				t.Fatalf("receipt=%+v err=%v closed=%v", r, err, closed)
			}
		})
	}
}

func TestReapTransportedReviewerIsIdempotentAfterExactClose(t *testing.T) {
	root := t.TempDir()
	artifact := reapArtifact(t, root)
	emitReapAck(t, root, artifact)
	live := true
	closed := 0
	installReapSeams(t, func() ([]herdr.AgentEntry, error) {
		if !live {
			return []herdr.AgentEntry{}, nil
		}
		return []herdr.AgentEntry{reapAgent("done", false)}, nil
	}, func(herdr.AgentEntry) error {
		closed++
		live = false
		return nil
	}, reapTestSHA)

	first, err := reapTransportedReviewer(root, artifact, false)
	if err != nil || first.Disposition != "reaped" {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	second, err := reapTransportedReviewer(root, artifact, false)
	if err != nil || second.Disposition != "already_absent" || closed != 1 {
		t.Fatalf("second=%+v err=%v closes=%d", second, err, closed)
	}
}

func TestReapTransportedReviewerDryRunReportsWithoutClosing(t *testing.T) {
	root := t.TempDir()
	artifact := reapArtifact(t, root)
	emitReapAck(t, root, artifact)
	closed := false
	installReapSeams(t, func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{reapAgent("idle", false)}, nil }, func(herdr.AgentEntry) error {
		closed = true
		return nil
	}, reapTestSHA)

	r, err := reapTransportedReviewer(root, artifact, true)
	if err != nil || r.Disposition != "would_reap" || closed {
		t.Fatalf("receipt=%+v err=%v closed=%v", r, err, closed)
	}
}

func TestReapTransportedReviewerRetainsWithoutIngestAck(t *testing.T) {
	root := t.TempDir()
	artifact := reapArtifact(t, root)
	closed := false
	installReapSeams(t, func() ([]herdr.AgentEntry, error) { return []herdr.AgentEntry{reapAgent("idle", false)}, nil }, func(herdr.AgentEntry) error {
		closed = true
		return nil
	}, reapTestSHA)
	r, err := reapTransportedReviewer(root, artifact, false)
	if err != nil || r.Disposition != "retained" || !strings.Contains(r.Reason, "ingest_ack") || closed {
		t.Fatalf("receipt=%+v err=%v closed=%v", r, err, closed)
	}
}

func TestVerdictSweepRemoteReadbackFailureNeverInspectsAgents(t *testing.T) {
	dir := initRepo(t)
	inbox := filepath.Join(dir, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		t.Fatal(err)
	}
	reapArtifact(t, inbox)
	t.Setenv("HERD_ROOT", dir)
	called := false
	installReapSeams(t, func() ([]herdr.AgentEntry, error) {
		called = true
		return nil, errors.New("must not be called")
	}, func(herdr.AgentEntry) error { return nil }, reapTestSHA)

	err := sweepVerdictPush("missing-remote", "w4", false)
	if err == nil || !strings.Contains(err.Error(), "list remote verdict refs") || called {
		t.Fatalf("err=%v agent_census_called=%v", err, called)
	}
}
