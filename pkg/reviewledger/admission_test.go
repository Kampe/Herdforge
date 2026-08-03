package reviewledger

import (
	"strings"
	"testing"
)

const (
	admitTask     = "FAC-146"
	admitLease    = "lease-gen-1"
	admitPatch    = "https://patch.example/1"
	admitDigest   = "sha256:deadbeef"
	admitAuthorFm = "anthropic"
	admitAuthorID = "author-session-1"
	admitReviewFm = "google"
	admitReviewer = "reviewer-1"
)

// admitBaseline writes a launch + verdict pair that satisfies every
// Admit() gate, and returns the AdmissionOpts a caller would assert for it.
func admitBaseline(t *testing.T, l *Ledger, sha string) AdmissionOpts {
	t.Helper()
	mustErr(l.Record(RecordOpts{
		SHA: sha, Branch: "main",
		BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
		Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
	}))
	must2(l.Verdict(VerdictOpts{
		SHA: sha, Reviewer: admitReviewer, Verdict: VerdictPASS,
		ReviewerFamily: admitReviewFm, Task: admitTask, Lease: admitLease,
		PatchURL: admitPatch, VfyDigest: admitDigest,
	}))
	return AdmissionOpts{
		CandidateSHA: sha, Task: admitTask, Lease: admitLease,
		PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
	}
}

// TestAdmit is the mutation-proof table: each case exercises exactly one
// gate. Deleting that gate's check in admission.go turns the matching
// wantAdmit=false case into a false positive, so the test fails.
func TestAdmit(t *testing.T) {
	tests := []struct {
		name       string
		setup      func(t *testing.T, l *Ledger) AdmissionOpts
		wantAdmit  bool
		wantReason string
	}{
		{
			name: "valid independent verdict admits (control case)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				return admitBaseline(t, l, "sha-valid")
			},
			wantAdmit: true,
		},
		// --- FAC-126 incident shape: author-family PASS does not admit ---
		// Two independent checks guard family self-verdict: the family
		// recorded on the launch row, and the family the caller separately
		// asserts as the author's. Each case below isolates one — the
		// other input is left non-matching so only the targeted gate can
		// catch it.
		{
			name: "reviewer family matches the launch-recorded author family (provenance gate: launch record)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-author-family-launch"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
					Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: admitReviewer, Verdict: VerdictPASS,
					ReviewerFamily: admitAuthorFm, // same family as the launch record's author
					Task:           admitTask, Lease: admitLease, PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorIdentity: admitAuthorID, // AuthorFamily deliberately unset
				}
			},
			wantAdmit: false, wantReason: "self-verdict",
		},
		{
			name: "reviewer family matches the caller-asserted author family though the launch record differs (provenance gate: asserted family)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-author-family-asserted"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: "openai", BuilderIdentity: admitAuthorID, // launch row disagrees
					Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: admitReviewer, Verdict: VerdictPASS,
					ReviewerFamily: admitAuthorFm, // matches the caller's asserted author family below
					Task:           admitTask, Lease: admitLease, PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "self-verdict",
		},
		{
			name: "reviewer identity matches the launch-recorded author identity, shared account (provenance gate: launch record)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-shared-identity-launch"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: "shared-account",
					Reviewer: "shared-account", Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: "shared-account", Verdict: VerdictPASS,
					ReviewerFamily: admitReviewFm, // different family, same actor
					Task:           admitTask, Lease: admitLease, PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, // AuthorIdentity deliberately unset
				}
			},
			wantAdmit: false, wantReason: "shared account",
		},
		{
			name: "reviewer identity matches the caller-asserted author identity though the launch record differs (provenance gate: asserted identity)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-shared-identity-asserted"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: "other-launch-identity", // launch row disagrees
					Reviewer: "shared-account", Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: "shared-account", Verdict: VerdictPASS,
					ReviewerFamily: admitReviewFm,
					Task:           admitTask, Lease: admitLease, PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: "shared-account",
				}
			},
			wantAdmit: false, wantReason: "shared account",
		},
		// --- FAC-126 incident shape: stale-SHA PASS does not admit ---
		{
			name: "PASS recorded against an old sha does not admit the new candidate sha (exact-sha gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				oldSHA := "sha-old-head"
				newSHA := "sha-new-head"
				// A valid PASS exists, but only for the superseded SHA.
				admitBaseline(t, l, oldSHA)
				return AdmissionOpts{
					CandidateSHA: newSHA, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "stale or unknown sha",
		},
		// --- FAC-126 incident shape: prose PASS does not admit ---
		{
			name: "reviewer still working (only a launch record, no structured verdict) does not admit — prose never substitutes",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-still-working"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
					Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "reviewer still working",
		},
		{
			name: "a verdict row whose Verdict field is prose (\"PASS bound to <sha>\") is not a PASS and does not admit",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-prose-verdict"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
					Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
				}))
				// Bypasses the Verdict() API/ValidVerdict — simulates a
				// non-ledger writer (e.g. a PR-comment monitor) trying to
				// smuggle prose into a verdict-shaped row.
				mustErr(l.appendRow(l.Path, &LedgerRow{
					Event: string(EventVerdict), SHA: sha, Reviewer: admitReviewer,
					Verdict: "PASS bound to " + sha, ReviewerFamily: admitReviewFm,
					Task: admitTask, Lease: admitLease, PatchURL: admitPatch,
					VerificationDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "no verdict satisfied",
		},
		// --- veto gate ---
		{
			name: "unsuperseded FAIL from another reviewer blocks admission even with a valid PASS present (veto gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-veto"
				opts := admitBaseline(t, l, sha)
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main", BuilderFamily: admitAuthorFm,
					Reviewer: "reviewer-2", Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: "reviewer-2", Verdict: VerdictFAIL,
					ReviewerFamily: "xai", Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return opts
			},
			wantAdmit: false, wantReason: "veto",
		},
		{
			name: "unsuperseded BLOCKED from another reviewer blocks admission (veto gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-blocked-veto"
				opts := admitBaseline(t, l, sha)
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main", BuilderFamily: admitAuthorFm,
					Reviewer: "reviewer-3", Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: "reviewer-3", Verdict: VerdictBLOCKED,
					ReviewerFamily: "xai", Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return opts
			},
			wantAdmit: false, wantReason: "veto",
		},
		// --- coordinator gate ---
		{
			name: "coordinator self-verdict never qualifies as merge authority",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-coordinator"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main", BuilderFamily: admitAuthorFm,
					Reviewer: "chainseer-orchestrator", Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: "chainseer-orchestrator", Verdict: VerdictPASS,
					ReviewerFamily: admitReviewFm, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "coordinator",
		},
		// --- exact-binding gates: task / lease / patch / digest / tier ---
		{
			name: "verdict bound to a different task does not admit (task binding gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-wrong-task"
				opts := admitBaseline(t, l, sha)
				opts.Task = "FAC-999"
				return opts
			},
			wantAdmit: false, wantReason: "task",
		},
		{
			name: "verdict bound to a superseded lease generation does not admit (lease binding gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-stale-lease"
				opts := admitBaseline(t, l, sha)
				opts.Lease = "lease-gen-2"
				return opts
			},
			wantAdmit: false, wantReason: "lease",
		},
		{
			name: "verdict bound to a different patch id does not admit (patch binding gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-wrong-patch"
				opts := admitBaseline(t, l, sha)
				opts.PatchURL = "https://patch.example/999"
				return opts
			},
			wantAdmit: false, wantReason: "patch id",
		},
		{
			name: "verdict missing a verification digest does not admit (verification-evidence gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-no-digest"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
					Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: admitReviewer, Verdict: VerdictPASS,
					ReviewerFamily: admitReviewFm, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, // VfyDigest intentionally omitted
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "verification digest",
		},
		{
			name: "no risk tier recorded for the candidate sha does not admit (risk-tier gate)",
			setup: func(t *testing.T, l *Ledger) AdmissionOpts {
				sha := "sha-no-tier"
				mustErr(l.Record(RecordOpts{
					SHA: sha, Branch: "main",
					BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
					Reviewer: admitReviewer, Task: admitTask, // Tier intentionally omitted
				}))
				must2(l.Verdict(VerdictOpts{
					SHA: sha, Reviewer: admitReviewer, Verdict: VerdictPASS,
					ReviewerFamily: admitReviewFm, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, VfyDigest: admitDigest,
				}))
				return AdmissionOpts{
					CandidateSHA: sha, Task: admitTask, Lease: admitLease,
					PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
				}
			},
			wantAdmit: false, wantReason: "risk tier",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLedger(t)
			opts := tc.setup(t, l)
			result, err := l.Admit(opts)
			if result == nil {
				t.Fatalf("Admit returned nil result")
			}
			if result.Admitted != tc.wantAdmit {
				t.Fatalf("Admitted = %v, want %v (reason=%q err=%v)", result.Admitted, tc.wantAdmit, result.Reason, err)
			}
			if tc.wantAdmit {
				if err != nil {
					t.Fatalf("unexpected error on admit: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("expected error on rejection, got nil")
				}
				if !strings.Contains(result.Reason, tc.wantReason) {
					t.Errorf("Reason = %q, want contains %q", result.Reason, tc.wantReason)
				}
			}
		})
	}
}

// TestAdmitRequiredOptsFieldsFailClosed exercises the "evidence is missing"
// gate directly: every AdmissionOpts field Admit binds on must be present.
func TestAdmitRequiredOptsFieldsFailClosed(t *testing.T) {
	base := AdmissionOpts{
		CandidateSHA: "sha-required", Task: admitTask,
		Lease: admitLease, PatchURL: admitPatch,
	}
	tests := []struct {
		name string
		opts AdmissionOpts
	}{
		{"missing candidate sha", func() AdmissionOpts { o := base; o.CandidateSHA = ""; return o }()},
		{"missing task", func() AdmissionOpts { o := base; o.Task = ""; return o }()},
		{"missing lease", func() AdmissionOpts { o := base; o.Lease = ""; return o }()},
		{"missing patch url", func() AdmissionOpts { o := base; o.PatchURL = ""; return o }()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l := newTestLedger(t)
			result, err := l.Admit(tc.opts)
			if result == nil || result.Admitted {
				t.Fatalf("expected rejection, got %+v", result)
			}
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestAdmitExactlyOnce proves a valid verdict admits, and after the caller
// marks the SHA Consumed, a second Admit call for the same SHA fails
// closed — the ledger record is consumed exactly once.
func TestAdmitExactlyOnce(t *testing.T) {
	l := newTestLedger(t)
	sha := "sha-exactly-once"
	opts := admitBaseline(t, l, sha)

	result, err := l.Admit(opts)
	if err != nil {
		t.Fatalf("first Admit: %v", err)
	}
	if !result.Admitted {
		t.Fatalf("expected first Admit to admit, got %+v", result)
	}

	mustErr(l.Consumed(result.SHA, "merged-sha-deadbeef"))

	result2, err := l.Admit(opts)
	if err == nil {
		t.Fatal("expected error on second Admit after consumption")
	}
	if result2 == nil || result2.Admitted {
		t.Fatalf("expected second Admit to refuse, got %+v", result2)
	}
	if !strings.Contains(result2.Reason, "already consumed") {
		t.Errorf("Reason = %q, want contains %q", result2.Reason, "already consumed")
	}
}

// TestAdmitReadsFreshFromDisk proves Admit never trusts cached/in-memory
// state — a verdict written after opts are captured is still picked up,
// since every Admit call re-reads the durable ledger from disk (readback).
func TestAdmitReadsFreshFromDisk(t *testing.T) {
	l := newTestLedger(t)
	sha := "sha-readback"
	mustErr(l.Record(RecordOpts{
		SHA: sha, Branch: "main",
		BuilderFamily: admitAuthorFm, BuilderIdentity: admitAuthorID,
		Reviewer: admitReviewer, Tier: "R2", Task: admitTask,
	}))
	opts := AdmissionOpts{
		CandidateSHA: sha, Task: admitTask, Lease: admitLease,
		PatchURL: admitPatch, AuthorFamily: admitAuthorFm, AuthorIdentity: admitAuthorID,
	}

	first, err := l.Admit(opts)
	if err == nil || first.Admitted {
		t.Fatalf("expected rejection before verdict is written, got %+v err=%v", first, err)
	}

	must2(l.Verdict(VerdictOpts{
		SHA: sha, Reviewer: admitReviewer, Verdict: VerdictPASS,
		ReviewerFamily: admitReviewFm, Task: admitTask, Lease: admitLease,
		PatchURL: admitPatch, VfyDigest: admitDigest,
	}))

	second, err := l.Admit(opts)
	if err != nil {
		t.Fatalf("Admit after durable verdict append: %v", err)
	}
	if !second.Admitted {
		t.Fatalf("expected admission after durable verdict append, got %+v", second)
	}
}
