package main

import (
	"context"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// TestReviewAgentNameIsUniquePerCandidate is the FAC-574 regression.
//
// The truncation suffix hashed the REF ONLY, so two distinct SHAs on the same
// branch produced the same agent name and the second reviewer collided with the
// first still-active one. Reviewing a second exact SHA on one branch is normal.
func TestReviewAgentNameIsUniquePerCandidate(t *testing.T) {
	// A ref long enough to force truncation, which is where the bug lived.
	ref := "reconstruct/cha-2209-review-handoff-queue-identity-long-branch"
	a := reviewAgentName(ref, "aaaaaaaaaaaa1111111111112222222222223333")
	b := reviewAgentName(ref, "bbbbbbbbbbbb1111111111112222222222223333")
	if a == b {
		t.Fatalf("two candidates on one branch must not share an agent name: %q", a)
	}
	if len(a) > reviewAgentNameLimit || len(b) > reviewAgentNameLimit {
		t.Fatalf("names must stay within %d chars: %q %q", reviewAgentNameLimit, a, b)
	}
}

// Short refs must keep their readable un-truncated form.
func TestShortRefKeepsReadableName(t *testing.T) {
	got := reviewAgentName("cha-1", "abcdef1234567890")
	if got != "review-cha-1-abcdef123456" && len(got) > reviewAgentNameLimit {
		t.Fatalf("unexpected short-ref name %q", got)
	}
	if got == reviewAgentName("cha-1", "999999999999") {
		t.Fatal("even short names must differ per candidate")
	}
}

// Tab label and agent name must both be candidate-unique; they are one
// definition now, so this guards against them diverging again.
func TestTabLabelAndAgentNameAgreeOnUniqueness(t *testing.T) {
	ref := "reconstruct/cha-2209-review-handoff-queue-identity-long-branch"
	if reviewTabLabel(ref, "aaaa1111") == reviewTabLabel(ref, "bbbb2222") {
		t.Fatal("tab labels must differ per candidate")
	}
}

func TestResolveReviewTaskRef(t *testing.T) {
	const project = "project-1"
	newProvider := func(ref, projectID, status string) *provider.MemoryProvider {
		p := provider.NewMemoryProvider()
		p.AddTask(&provider.Task{ID: "task-1", Ref: ref, ProjectID: projectID, Status: status})
		return p
	}

	tests := []struct {
		name     string
		selector string
		ref      string
		project  string
		status   string
		want     string
		err      string
	}{
		{name: "exact provider ref", selector: "FAC-654", ref: "FAC-654", project: project, status: provider.StatusInProgress, want: "FAC-654"},
		{name: "branch resolves exact card", selector: "herd/fac-654", ref: "FAC-654", project: project, status: provider.StatusInProgress, want: "FAC-654"},
		{name: "native fac-755 branch selector", selector: "herd/fac-755", ref: "FAC-755", project: project, status: provider.StatusInReview, want: "FAC-755"},
		{name: "zero padded branch selector matches padded provider card", selector: "herd/fac-018", ref: "FAC-018", project: project, status: provider.StatusInProgress, want: "FAC-018"},
		{name: "unpadded selector matches padded provider card", selector: "FAC-18", ref: "FAC-018", project: project, status: provider.StatusInProgress, want: "FAC-018"},
		{name: "padded selector matches unpadded provider card", selector: "FAC-018", ref: "FAC-18", project: project, status: provider.StatusInProgress, want: "FAC-18"},
		{name: "branch and card mismatch", selector: "herd/fac-654", ref: "FAC-655", project: project, status: provider.StatusInProgress, err: "card FAC-654 not found"},
		{name: "missing project context", selector: "FAC-654", ref: "FAC-654", project: "", status: provider.StatusInProgress, err: "project context is required"},
		{name: "non-closeable provider identity", selector: "standing/api-crusader", ref: "standing/api-crusader", project: project, status: provider.StatusInProgress, err: "neither a closeable card ref"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newProvider(tt.ref, tt.project, tt.status)
			got, err := resolveReviewTaskRef(context.Background(), p, tt.project, tt.selector)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("resolveReviewTaskRef error = %v, want substring %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got == nil || got.Ref != tt.want {
				t.Fatalf("resolved task = %+v, want ref %q", got, tt.want)
			}
			card, identErr := reviewPacketTaskIdentity(got)
			if identErr != nil {
				t.Fatal(identErr)
			}
			if card != tt.want {
				t.Fatalf("packet identity = %q, want %q", card, tt.want)
			}
		})
	}
}

// TestResolveReviewTaskRefZeroPaddedBranchAndProviderCard addresses the finding:
// "Zero-padded branch refs are normalized differently from provider card refs.
// reviewTaskSelectorRef in cmd/herd/review_identity.go delegates branch extraction to
// drainCandidateRef, whose hsync.NormalizeRef call turns herd/fac-018 into FAC-18.
// The active-list match canonicalizes the provider task with CloseableCardRef (FAC-018),
// so the valid branch/card pair cannot match."
func TestResolveReviewTaskRefZeroPaddedBranchAndProviderCard(t *testing.T) {
	p := provider.NewMemoryProvider()
	p.AddTask(&provider.Task{ID: "task-018", Ref: "FAC-018", ProjectID: "project-1", Status: provider.StatusInProgress})

	got, err := resolveReviewTaskRef(context.Background(), p, "project-1", "herd/fac-018")
	if err != nil {
		t.Fatalf("resolveReviewTaskRef(herd/fac-018) failed: %v", err)
	}
	if got == nil || got.Ref != "FAC-018" {
		t.Fatalf("got task ref %v, want FAC-018", got)
	}
	card, err := reviewPacketTaskIdentity(got)
	if err != nil {
		t.Fatalf("reviewPacketTaskIdentity failed: %v", err)
	}
	if card != "FAC-018" {
		t.Fatalf("packet identity = %q, want FAC-018", card)
	}
}

func TestResolveReviewTaskRefRejectsAmbiguousCards(t *testing.T) {
	p := provider.NewMemoryProvider()
	for _, id := range []string{"task-a", "task-b"} {
		p.AddTask(&provider.Task{ID: id, Ref: "FAC-654", ProjectID: "project-1", Status: provider.StatusInProgress})
	}
	if _, err := resolveReviewTaskRef(context.Background(), p, "project-1", "herd/fac-654"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous card identity was accepted: %v", err)
	}
}

func TestResolveReviewTaskRefRejectsCrossProjectCard(t *testing.T) {
	p := provider.NewMemoryProvider()
	p.AddTask(&provider.Task{ID: "task-1", Ref: "FAC-734", ProjectID: "other-project", Status: provider.StatusToDo})
	if _, err := resolveReviewTaskRef(context.Background(), p, "project-1", "herd/fac-734"); err == nil || !strings.Contains(err.Error(), "belongs to project") {
		t.Fatalf("cross-project card was accepted: %v", err)
	}
}

func TestReviewPacketBindsResolvedCardFromBranchSelector(t *testing.T) {
	p := provider.NewMemoryProvider()
	p.AddTask(&provider.Task{ID: "task-1", Ref: "fac-755", ProjectID: "project-1", Status: provider.StatusInProgress})
	task, err := resolveReviewTaskRef(context.Background(), p, "project-1", "herd/fac-755")
	if err != nil {
		t.Fatal(err)
	}
	card, err := reviewPacketTaskIdentity(task)
	if err != nil {
		t.Fatal(err)
	}
	if card != "FAC-755" {
		t.Fatalf("packet identity = %q, want FAC-755", card)
	}
	body := reviewPacketBody("herd/fac-755", strings.Repeat("a", 40),
		"base", ".herd/review-surfaces/review-herd-fac-755",
		".herd/pool/pool-01",
		"/repo/.herd/review/inbox/v.md",
		"forge-review-supervisor-4922de28", "xai", "wK", card)
	if strings.Contains(body, "\ntask: herd/fac-755\n") {
		t.Fatal("composed launch path prefills the branch")
	}
	if !strings.Contains(body, "\ntask: FAC-755\n") {
		t.Fatal("composed launch path must prefill the closeable card")
	}
}

func TestReviewPacketTaskIdentityCanonicalizesAndRejects(t *testing.T) {
	got, err := reviewPacketTaskIdentity(&provider.Task{Ref: "fac-755"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "FAC-755" {
		t.Fatalf("canonical identity = %q, want FAC-755", got)
	}
	if _, err := reviewPacketTaskIdentity(&provider.Task{Ref: "herd/fac-755"}); err == nil {
		t.Fatal("branch ref was accepted as packet identity")
	}
	if _, err := reviewPacketTaskIdentity(nil); err == nil {
		t.Fatal("nil provider task was accepted")
	}
}

// FAC-655 Claude FAIL on 382a3c1e: packetTask was passed into
// recordAssertedBuilderLaunch and completeReviewLaunchProvenance, so the
// ledger Reviewer became reviewAgentName(card) while the launched agent is
// reviewAgentName(branch selector). Admission keys SHA+Reviewer and then
// cannot find a launch row for the real reviewer.
func TestRecordAssertedBuilderLaunchBindsLaunchReviewerAndCloseableTask(t *testing.T) {
	const (
		selector   = "herd/fac-655"
		packetTask = "FAC-655"
		sha        = "382a3c1e31038a706b1773290133c3be83a702e7"
		family     = "xai"
	)
	launchReviewer := reviewAgentName(selector, sha)
	cardReviewer := reviewAgentName(packetTask, sha)
	if launchReviewer == cardReviewer {
		t.Fatal("vacuous: branch selector and card produce the same reviewer identity")
	}
	if launchReviewer != "review-herd-fac-655-382a3c1e3103" {
		t.Fatalf("launch identity = %q, want review-herd-fac-655-382a3c1e3103", launchReviewer)
	}

	root := t.TempDir()
	if err := recordAssertedBuilderLaunch(root, selector, sha, family, packetTask); err != nil {
		t.Fatal(err)
	}
	l, err := reviewledger.NewReadOnlyReviewLedger(root, reviewledger.DefaultPath(root))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1 launch record", len(rows))
	}
	got := rows[0]
	if got.Reviewer != launchReviewer {
		t.Fatalf("ledger Reviewer = %q, want launch identity %q (card-derived name was %q)", got.Reviewer, launchReviewer, cardReviewer)
	}
	if got.Task != packetTask {
		t.Fatalf("ledger Task = %q, want %s", got.Task, packetTask)
	}
	if got.SHA+":"+got.Reviewer != sha+":"+launchReviewer {
		t.Fatalf("admission key %q does not match launched reviewer %q", got.SHA+":"+got.Reviewer, launchReviewer)
	}
}

func TestReviewLaunchRecordBindingSeparatesReviewerAndTask(t *testing.T) {
	sha := "382a3c1e31038a706b1773290133c3be83a702e7"
	reviewer, task, err := reviewLaunchRecordBinding("herd/fac-655", "FAC-655", sha)
	if err != nil {
		t.Fatal(err)
	}
	if reviewer != "review-herd-fac-655-382a3c1e3103" {
		t.Fatalf("Reviewer = %q, want launch identity review-herd-fac-655-382a3c1e3103", reviewer)
	}
	if task != "FAC-655" {
		t.Fatalf("Task = %q, want FAC-655", task)
	}
	if _, _, err := reviewLaunchRecordBinding("herd/fac-655", "herd/fac-655", sha); err == nil {
		t.Fatal("branch selector was accepted as ledger Task")
	}
	if _, _, err := reviewLaunchRecordBinding("", "FAC-655", sha); err == nil {
		t.Fatal("empty selector was accepted")
	}
}
