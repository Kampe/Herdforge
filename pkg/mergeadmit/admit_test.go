package mergeadmit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

const (
	testRef      = "FAC-156"
	testTaskID   = "p80wxhmltmkt4d4ad184l3q5"
	testRevision = "rev-7"
	testLease    = "lease-3"
	testPatchURL = "patch-abcdef"
	testVfy      = "vfy-digest-1"
	testCheck    = "Build, Preflight & Test Suite"
)

func testPolicy() preflight.MergePolicy {
	return preflight.MergePolicy{
		Protected:                    true,
		RequiredChecks:               []string{testCheck},
		RequireDifferentFamilyReview: true,
		RequirePullRequestReviews:    true,
	}
}

func newLedger(t *testing.T, dir string) *reviewledger.Ledger {
	t.Helper()
	l, err := reviewledger.NewReviewLedger(dir, filepath.Join(dir, "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	return l
}

// launch records the reviewer dispatch for a candidate. Admit requires one:
// a verdict with no launch record has no provable provenance.
func launch(t *testing.T, l *reviewledger.Ledger, sha, reviewer, builderFam, builderID string) {
	t.Helper()
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Reviewer: reviewer, BuilderFamily: builderFam, BuilderIdentity: builderID,
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3", Task: testRef, Lease: testLease,
	}); err != nil {
		t.Fatalf("record launch: %v", err)
	}
}

func verdict(t *testing.T, l *reviewledger.Ledger, sha, reviewer string, v reviewledger.Verdict) {
	t.Helper()
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: reviewer, Verdict: v, ReviewerFamily: "openai", BuilderFamily: "anthropic",
		Task: testRef, Lease: testLease, PatchURL: testPatchURL, VfyDigest: testVfy,
		Artifact: "verdict.md", CandidateSHA: sha,
	}); err != nil {
		t.Fatalf("write verdict: %v", err)
	}
}

// okGate builds a gate whose every live probe reports the healthy state, so a
// test only has to break the ONE thing it is about.
func okGate(t *testing.T, l *reviewledger.Ledger, base, head string) *Gate {
	t.Helper()
	return &Gate{
		RepoDir: t.TempDir(),
		Ledger:  l,
		Policy:  testPolicy(),
		Live: LiveState{
			OriginMain:    StaticProbe(base),
			CandidateHead: StaticProbe(head),
			Mergeable:     StaticProbe("CLEAN"),
			TaskRevision:  StaticProbe(testRevision),
			Checks:        func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
		},
	}
}

func okRequest(base, candidate string) Request {
	return Request{
		Ref: testRef, TaskID: testTaskID, ProviderRevision: testRevision,
		AcceptanceDigest: ComputeAcceptanceDigest(testRef, testTaskID, testRevision),
		CandidateSHA:     candidate, BaseSHA: base,
		Lease: testLease, LeaseGeneration: 3, PatchURL: testPatchURL,
		AuthorFamily: "anthropic", AuthorIdentity: "builder-session-1",
		Mode: ModeMerge,
	}
}

const (
	shaOld     = "0149e5e0000000000000000000000000000000aa"
	shaCurrent = "d11b37b0000000000000000000000000000000bb"
	shaBase    = "b0000000000000000000000000000000000000cc"
)

func mustAdmit(t *testing.T, g *Gate, req Request) *Decision {
	t.Helper()
	d, err := g.Admit(req)
	if err != nil || d == nil || !d.Admitted {
		t.Fatalf("expected admission, got admitted=%v err=%v", d != nil && d.Admitted, err)
	}
	return d
}

func mustRefuse(t *testing.T, g *Gate, req Request, wantCode string) *Decision {
	t.Helper()
	d, err := g.Admit(req)
	if d == nil {
		t.Fatalf("refusal returned no decision (err=%v); callers that gate on the decision would see nothing", err)
	}
	if d.Admitted {
		t.Fatalf("gate ADMITTED a candidate it must refuse (wanted code %s)", wantCode)
	}
	if err == nil {
		t.Fatal("refusal returned a nil error; a caller using `if err != nil` would read it as consent")
	}
	if d.Code != wantCode {
		t.Fatalf("refusal code = %q, want %q (reason: %s)", d.Code, wantCode, d.Reason)
	}
	return d
}

// ACCEPTANCE CRITERION 1 — the FAC-149/PR#65 incident, verbatim.
//
// The coordinator grepped PR comments and took the FIRST PASS, which belonged
// to superseded candidate 0149e5e. The corrected PASS for the current
// candidate d11b37b was later in the list and never selected.
//
// A verdict is bound to the exact sha it reviewed. The old-sha PASS must be
// invisible to a merge of the current sha, and the current sha must stand or
// fall on its own record.
func TestAdmitSelectsByExactSHANotFirstPass(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)

	// Append order is the incident's: the stale PASS lands first.
	launch(t, l, shaOld, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaOld, "reviewer-a", reviewledger.VerdictPASS)
	launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	d := mustAdmit(t, g, okRequest(shaBase, shaCurrent))
	if d.CandidateSHA != shaCurrent {
		t.Fatalf("admitted %s, want the current candidate %s", d.CandidateSHA, shaCurrent)
	}

	// The decisive half: the stale candidate's own PASS still exists in the
	// ledger, but merging THAT sha is refused because the live head is the
	// current one. First-match selection would have merged it.
	g2 := okGate(t, l, shaBase, shaCurrent)
	mustRefuse(t, g2, okRequest(shaBase, shaOld), CodeHeadMoved)
}

// appendRawRow appends a ledger row directly, bypassing Ledger.Verdict's
// same-(sha,reviewer) idempotency guard.
//
// This is necessary, not a shortcut. Ledger.Verdict drops any second verdict
// from the same reviewer for the same sha, so a reviewer CANNOT correct their
// own FAIL through the normal write path. The corrected-PASS shape that
// acceptance criterion 1 names is therefore unreachable via the API, and the
// only way to test that Admit's supersession selection is correct is to
// construct the row sequence the criterion describes. See
// TestAdmitSupersessionSelectsLastVerdictPerReviewer.
func appendRawRow(t *testing.T, dir string, row reviewledger.LedgerRow) {
	t.Helper()
	if row.Timestamp == "" {
		row.Timestamp = "2026-08-07T00:00:00Z"
	}
	b, err := json.Marshal(row)
	if err != nil {
		t.Fatalf("encode ledger row: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, "review-ledger.jsonl"),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatalf("append ledger row: %v", err)
	}
}

func rawLaunch(sha, reviewer string) reviewledger.LedgerRow {
	return reviewledger.LedgerRow{
		Event: string(reviewledger.EventRecord), SHA: sha, Reviewer: reviewer,
		BuilderFamily: "anthropic", BuilderIdentity: "builder-session-1",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R3",
		Task: testRef, Lease: testLease,
	}
}

func rawVerdict(sha, reviewer string, v reviewledger.Verdict) reviewledger.LedgerRow {
	return reviewledger.LedgerRow{
		Event: string(reviewledger.EventVerdict), SHA: sha, Reviewer: reviewer,
		Verdict: string(v), ReviewerFamily: "openai", BuilderFamily: "anthropic",
		Task: testRef, Lease: testLease, PatchURL: testPatchURL,
		VerificationDigest: testVfy, CandidateSHA: sha,
	}
}

// ACCEPTANCE CRITERION 1, the exact three-verdict sequence:
//
//	old-sha PASS  →  current-sha FAIL  →  current-sha corrected PASS
//
// Selection is by exact sha AND by append order within a reviewer. The old-sha
// PASS is irrelevant to the current candidate, the FAIL is superseded by the
// same reviewer's later PASS, and only the final current verdict decides.
//
// Removing the exact-sha filter admits on the first PASS in the file (the
// stale one, which is the FAC-149 incident). Removing last-wins supersession
// leaves the FAIL standing and refuses. Both mutations are exercised below.
func TestAdmitSupersessionSelectsLastVerdictPerReviewer(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)

	appendRawRow(t, dir, rawLaunch(shaOld, "reviewer-a"))
	appendRawRow(t, dir, rawVerdict(shaOld, "reviewer-a", reviewledger.VerdictPASS))
	appendRawRow(t, dir, rawLaunch(shaCurrent, "reviewer-a"))
	appendRawRow(t, dir, rawVerdict(shaCurrent, "reviewer-a", reviewledger.VerdictFAIL))
	appendRawRow(t, dir, rawVerdict(shaCurrent, "reviewer-a", reviewledger.VerdictPASS))

	g := okGate(t, l, shaBase, shaCurrent)
	d := mustAdmit(t, g, okRequest(shaBase, shaCurrent))
	if d.CandidateSHA != shaCurrent {
		t.Fatalf("admitted %s, want the final current candidate %s", d.CandidateSHA, shaCurrent)
	}

	// Order is what carries the correction. With the PASS and FAIL swapped,
	// the standing verdict is a veto and the same ledger must refuse.
	dir2 := t.TempDir()
	l2 := newLedger(t, dir2)
	appendRawRow(t, dir2, rawLaunch(shaOld, "reviewer-a"))
	appendRawRow(t, dir2, rawVerdict(shaOld, "reviewer-a", reviewledger.VerdictPASS))
	appendRawRow(t, dir2, rawLaunch(shaCurrent, "reviewer-a"))
	appendRawRow(t, dir2, rawVerdict(shaCurrent, "reviewer-a", reviewledger.VerdictPASS))
	appendRawRow(t, dir2, rawVerdict(shaCurrent, "reviewer-a", reviewledger.VerdictFAIL))

	g2 := okGate(t, l2, shaBase, shaCurrent)
	mustRefuse(t, g2, okRequest(shaBase, shaCurrent), CodeLedgerRefused)
}

// KNOWN GAP, pinned so it cannot rot silently.
//
// Ledger.Verdict is idempotent per (sha, reviewer): a second verdict from the
// same reviewer for the same sha is DROPPED. Admit's supersession is
// last-wins, so it would honour a correction — but the write path cannot emit
// one, which makes the "fresh admissible PASS clears a veto" half of the card
// unreachable through the API.
//
// This test asserts the CURRENT behaviour. When the write path is fixed
// (a separate change, with its own blast radius across Eligible, PassSHAs and
// outstandingRejections), this test fails and must be inverted.
func TestLedgerWritePathCannotSupersedeAReviewersOwnVerdict(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictFAIL)
	// The same reviewer tries to correct themselves to PASS.
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	d := mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeLedgerRefused)
	if !strings.Contains(d.Reason, "FAIL") {
		t.Fatalf("expected the dropped correction to leave the FAIL standing, got: %s", d.Reason)
	}
}

// A PASS for one sha grants no authority over another sha, even when the other
// sha is the one everyone is talking about.
func TestAdmitRefusesWhenOnlyAnotherSHAHasAPass(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	launch(t, l, shaOld, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaOld, "reviewer-a", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	d := mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeLedgerRefused)
	if !strings.Contains(d.Reason, "no launch record") && !strings.Contains(d.Reason, "no verdict") {
		t.Fatalf("refusal reason should name the missing exact-sha record: %s", d.Reason)
	}
}

// ACCEPTANCE CRITERION 2 — a PASS followed by FAIL/BLOCKED for the same
// candidate cannot merge. The FAIL is not "an older opinion"; it is a veto
// standing against this exact candidate.
func TestAdmitRefusesPassThenFailSameCandidate(t *testing.T) {
	for _, veto := range []reviewledger.Verdict{reviewledger.VerdictFAIL, reviewledger.VerdictBLOCKED} {
		t.Run(string(veto), func(t *testing.T) {
			dir := t.TempDir()
			l := newLedger(t, dir)
			launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
			verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
			// A second reviewer vetoes the same candidate.
			launch(t, l, shaCurrent, "reviewer-b", "anthropic", "builder-session-1")
			verdict(t, l, shaCurrent, "reviewer-b", veto)

			g := okGate(t, l, shaBase, shaCurrent)
			d := mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeLedgerRefused)
			if !strings.Contains(d.Reason, string(veto)) {
				t.Fatalf("refusal did not cite the %s veto: %s", veto, d.Reason)
			}
		})
	}
}

// ACCEPTANCE CRITERION 3 — the FAC-150/PR#66 incident. A verdict that is
// internally valid but was minted against an older revision of the card does
// not satisfy the card's current acceptance criteria.
func TestAdmitRefusesStaleAcceptanceDigest(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	req := okRequest(shaBase, shaCurrent)
	// Reviewed against an earlier revision of the card.
	req.AcceptanceDigest = ComputeAcceptanceDigest(testRef, testTaskID, "rev-6")
	mustRefuse(t, g, req, CodeAcceptance)

	// An empty digest is not a wildcard either.
	req.AcceptanceDigest = ""
	mustRefuse(t, g, req, CodeMissingField)
}

// A card edited AFTER review is a different card. The caller's assertion is
// self-consistent here — it reviewed rev-7 and says so — and the refusal comes
// entirely from comparing it against what the board reports now.
func TestAdmitRefusesWhenCardEditedAfterReview(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	g.Live.TaskRevision = StaticProbe("rev-8") // the card was edited after review
	req := okRequest(shaBase, shaCurrent)      // caller still asserts rev-7, self-consistently
	mustRefuse(t, g, req, CodeTaskRevision)
}

// ACCEPTANCE CRITERION 5 — advancing origin/main, the PR head, the task
// revision, or the CI state after admission invalidates the merge attempt.
func TestAdmitRefusesWhenLiveStateAdvanced(t *testing.T) {
	newFixture := func(t *testing.T) (*Gate, Request) {
		dir := t.TempDir()
		l := newLedger(t, dir)
		launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
		verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
		return okGate(t, l, shaBase, shaCurrent), okRequest(shaBase, shaCurrent)
	}

	// The serial-train case: merging another candidate advanced origin/main,
	// which stales every queued receipt cut against the old base.
	t.Run("origin main advanced", func(t *testing.T) {
		g, req := newFixture(t)
		g.Live.OriginMain = StaticProbe("9907722000000000000000000000000000000000")
		d := mustRefuse(t, g, req, CodeBaseAdvanced)
		if _, err := g.Admit(req); !errors.Is(err, ErrRestartAdmission) {
			t.Fatal("a base advance must be signalled as restart-admission, not as a candidate defect")
		}
		if !strings.Contains(d.Reason, "stale") {
			t.Fatalf("reason should tell the caller its receipts are stale: %s", d.Reason)
		}
	})

	t.Run("candidate head moved", func(t *testing.T) {
		g, req := newFixture(t)
		g.Live.CandidateHead = StaticProbe("aaaaaaa0000000000000000000000000000000ff")
		mustRefuse(t, g, req, CodeHeadMoved)
		if _, err := g.Admit(req); !errors.Is(err, ErrRestartAdmission) {
			t.Fatal("a moved head must be signalled as restart-admission")
		}
	})

	t.Run("task revision moved", func(t *testing.T) {
		g, req := newFixture(t)
		g.Live.TaskRevision = StaticProbe("rev-99")
		mustRefuse(t, g, req, CodeTaskRevision)
	})

	t.Run("not mergeable", func(t *testing.T) {
		g, req := newFixture(t)
		g.Live.Mergeable = StaticProbe("CONFLICTING")
		mustRefuse(t, g, req, CodeNotMergeable)
	})

	t.Run("required check failed", func(t *testing.T) {
		g, req := newFixture(t)
		g.Live.Checks = func() (map[string]string, error) {
			return map[string]string{testCheck: "failure"}, nil
		}
		mustRefuse(t, g, req, CodeRequiredCheck)
	})

	// The claim was re-leased under a new generation while the verdict was in
	// flight. A verdict is bound to the lease it was produced under; a new
	// generation means a different claim on the same work.
	t.Run("lease generation rotated", func(t *testing.T) {
		g, req := newFixture(t)
		req.Lease = "lease-4"
		d := mustRefuse(t, g, req, CodeLedgerRefused)
		if !strings.Contains(d.Reason, "lease") {
			t.Fatalf("refusal did not cite the stale lease: %s", d.Reason)
		}
	})

	// The patch identity the reviewer bound their verdict to must still be the
	// candidate's. A rewritten patch under the same sha claim is not reviewed.
	t.Run("patch identity changed", func(t *testing.T) {
		g, req := newFixture(t)
		req.PatchURL = "patch-something-else"
		d := mustRefuse(t, g, req, CodeLedgerRefused)
		if !strings.Contains(d.Reason, "patch") {
			t.Fatalf("refusal did not cite the patch mismatch: %s", d.Reason)
		}
	})

	t.Run("required check absent", func(t *testing.T) {
		g, req := newFixture(t)
		g.Live.Checks = func() (map[string]string, error) {
			return map[string]string{"some other job": "success"}, nil
		}
		d := mustRefuse(t, g, req, CodeRequiredCheck)
		if !strings.Contains(d.Reason, "did not run") {
			t.Fatalf("an absent check must not read as a passed check: %s", d.Reason)
		}
	})
}

// THE FAC-162 MUTANT. The live probe was `gh pr view | jq | head`: jq and head
// exit 0 over an empty stream, so a failed gh produced ("" , nil) and the gate
// read it as "no blocking condition".
//
// Every probe must refuse on BOTH a producer error and an empty value. This
// table walks each probe through both shapes; if any probe stops failing
// closed, one of these subtests admits a merge it must refuse.
func TestAdmitProbeFailureNeverReadsAsAbsentCondition(t *testing.T) {
	probes := []struct {
		name string
		set  func(*Gate, Probe)
	}{
		{"origin_main", func(g *Gate, p Probe) { g.Live.OriginMain = p }},
		{"candidate_head", func(g *Gate, p Probe) { g.Live.CandidateHead = p }},
		{"mergeable", func(g *Gate, p Probe) { g.Live.Mergeable = p }},
		{"task_revision", func(g *Gate, p Probe) { g.Live.TaskRevision = p }},
	}
	broken := map[string]Probe{
		"producer failed": FailingProbe(fmt.Errorf("gh: HTTP 401 Bad credentials")),
		"empty from pipe": StaticProbe(""),
		"whitespace only": StaticProbe("  \n "),
		"probe unwired":   nil,
	}

	for _, pr := range probes {
		for shape, bad := range broken {
			t.Run(pr.name+"/"+shape, func(t *testing.T) {
				dir := t.TempDir()
				l := newLedger(t, dir)
				launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
				verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
				g := okGate(t, l, shaBase, shaCurrent)
				pr.set(g, bad)
				mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeProbeFailed)
			})
		}
	}
}

// The checks probe has the same failure shape, including the one that started
// the incident: an empty result set reported with no error at all.
func TestAdmitChecksProbeFailsClosed(t *testing.T) {
	for name, bad := range map[string]ChecksProbe{
		"producer failed": func() (map[string]string, error) { return nil, fmt.Errorf("gh: HTTP 502") },
		"empty set":       func() (map[string]string, error) { return map[string]string{}, nil },
		"nil map":         func() (map[string]string, error) { return nil, nil },
		"unwired":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l := newLedger(t, dir)
			launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
			verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
			g := okGate(t, l, shaBase, shaCurrent)
			g.Live.Checks = bad
			mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeRequiredCheck)
		})
	}
}

// A conclusion this build has never heard of must not widen the gate.
func TestAdmitRefusesUnknownCheckConclusion(t *testing.T) {
	for _, conclusion := range []string{"pending", "", "skipped", "cancelled", "timed_out", "action_required", "brand_new_state"} {
		dir := t.TempDir()
		l := newLedger(t, dir)
		launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
		verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
		g := okGate(t, l, shaBase, shaCurrent)
		g.Live.Checks = func() (map[string]string, error) {
			return map[string]string{testCheck: conclusion}, nil
		}
		mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeRequiredCheck)
	}
}

// Every asserted field is load-bearing. Dropping any one of them is a claim
// the caller cannot make, and the gate must not fill it in.
func TestAdmitRequiredFieldsFailClosed(t *testing.T) {
	blank := map[string]func(*Request){
		"ref":               func(r *Request) { r.Ref = "" },
		"task_id":           func(r *Request) { r.TaskID = "" },
		"provider_revision": func(r *Request) { r.ProviderRevision = "" },
		"acceptance_digest": func(r *Request) { r.AcceptanceDigest = "" },
		"candidate_sha":     func(r *Request) { r.CandidateSHA = "" },
		"base_sha":          func(r *Request) { r.BaseSHA = "" },
		"lease":             func(r *Request) { r.Lease = "" },
		"patch_url":         func(r *Request) { r.PatchURL = "" },
		"author_family":     func(r *Request) { r.AuthorFamily = "" },
		"author_identity":   func(r *Request) { r.AuthorIdentity = "" },
		"lease_generation":  func(r *Request) { r.LeaseGeneration = 0 },
		"mode":              func(r *Request) { r.Mode = "" },
	}
	for name, blankOut := range blank {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			l := newLedger(t, dir)
			launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
			verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
			g := okGate(t, l, shaBase, shaCurrent)
			req := okRequest(shaBase, shaCurrent)
			blankOut(&req)
			mustRefuse(t, g, req, CodeMissingField)
		})
	}
}

// No ledger means no verdict, and no verdict is not a PASS.
func TestAdmitWithoutLedgerRefuses(t *testing.T) {
	g := &Gate{RepoDir: t.TempDir(), Policy: testPolicy(), Live: LiveState{
		OriginMain: StaticProbe(shaBase), CandidateHead: StaticProbe(shaCurrent),
		Mergeable: StaticProbe("CLEAN"), TaskRevision: StaticProbe(testRevision),
		Checks: func() (map[string]string, error) { return map[string]string{testCheck: "success"}, nil },
	}}
	mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeMissingField)
}

// A repository that has not declared its merge contract cannot be merged into
// autonomously, however good the candidate is (FAC-135).
func TestAdmitRefusesUndeclaredMergePolicy(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)
	g := okGate(t, l, shaBase, shaCurrent)
	g.Policy = preflight.MergePolicy{Protected: true} // declares no required checks
	mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeNotDeclared)
}

// Exactly-once: a spent admission never merges twice.
func TestAdmitIsSpentExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	launch(t, l, shaCurrent, "reviewer-a", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "reviewer-a", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	mustAdmit(t, g, okRequest(shaBase, shaCurrent))
	if err := l.Consumed(shaCurrent, "f00d000000000000000000000000000000000000"); err != nil {
		t.Fatalf("consume: %v", err)
	}
	mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeLedgerRefused)
}

// A self-review never merges, whatever the family label says.
func TestAdmitRefusesSelfVerdict(t *testing.T) {
	dir := t.TempDir()
	l := newLedger(t, dir)
	// Reviewer identity is the builder's own session.
	launch(t, l, shaCurrent, "builder-session-1", "anthropic", "builder-session-1")
	verdict(t, l, shaCurrent, "builder-session-1", reviewledger.VerdictPASS)

	g := okGate(t, l, shaBase, shaCurrent)
	mustRefuse(t, g, okRequest(shaBase, shaCurrent), CodeLedgerRefused)
}

func TestSameSHARejectsEmptyAndTooShort(t *testing.T) {
	if sameSHA("", "") || sameSHA("abc", "abcdef0123") || sameSHA(shaCurrent, "") {
		t.Fatal("sameSHA treated an empty or too-short value as an identity claim")
	}
	if !sameSHA(shaCurrent, shaCurrent[:12]) || !sameSHA(strings.ToUpper(shaCurrent), shaCurrent) {
		t.Fatal("sameSHA failed to match an abbreviated or differently-cased id")
	}
}
