package reviewledger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ledgerWith(t *testing.T, lines ...string) *Ledger {
	t.Helper()
	root := t.TempDir()
	p := filepath.Join(root, "review-ledger.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewReadOnlyReviewLedger(root, p)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// The exact mistake this exists to prevent: rows are PRESENT, so a row count
// says "reviewed", while the verdict says FAIL. Eight candidates were reported
// merge-ready this way.
func TestMergeReadiness_RowsPresentButVerdictIsFail(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"record","sha":"aaa","reviewer":"r1"}`,
		`{"event":"verdict","sha":"aaa","reviewer":"r1","verdict":"FAIL"}`)
	got, err := l.MergeReadinessFor("aaa")
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready {
		t.Fatalf("a FAIL verdict must never be merge-ready: %+v", got)
	}
	if got.Failures != 1 {
		t.Fatalf("failures=%d, want 1", got.Failures)
	}
}

// #3203: FAIL from one reviewer, PASS from another, same timestamp. VerdictFor
// returns last-wins and would report PASS, discarding the dissent.
func TestMergeReadiness_SplitDecisionIsNotAPass(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"verdict","sha":"bbb","reviewer":"pool-04","verdict":"FAIL"}`,
		`{"event":"verdict","sha":"bbb","reviewer":"pool-06","verdict":"PASS"}`)
	got, _ := l.MergeReadinessFor("bbb")
	if got.Ready {
		t.Fatalf("a split decision must not be merge-ready: %+v", got)
	}
	if !strings.Contains(got.Reason, "disagree") {
		t.Errorf("reason must name the disagreement: %q", got.Reason)
	}
}

// BLOCKED is stronger than FAIL and must block even alongside a PASS.
func TestMergeReadiness_BlockedAlwaysBlocks(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"verdict","sha":"ccc","reviewer":"r1","verdict":"PASS"}`,
		`{"event":"verdict","sha":"ccc","reviewer":"r2","verdict":"BLOCKED"}`)
	got, _ := l.MergeReadinessFor("ccc")
	if got.Ready {
		t.Fatalf("BLOCKED must block: %+v", got)
	}
}

// A later verdict from the SAME reviewer supersedes its earlier one, so a fixed
// candidate can become ready without a second reviewer.
func TestMergeReadiness_SameReviewerSupersedes(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"record","sha":"ddd","reviewer":"r1","builder_family":"anthropic","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"ddd","reviewer":"r1","verdict":"FAIL","builder_family":"anthropic","reviewer_family":"xai","verification_digest":"digest"}`,
		`{"event":"verdict","sha":"ddd","reviewer":"r1","verdict":"PASS","builder_family":"anthropic","reviewer_family":"xai","verification_digest":"digest"}`)
	got, _ := l.MergeReadinessFor("ddd")
	if !got.Ready {
		t.Fatalf("a reviewer's own later PASS must supersede its FAIL: %+v", got)
	}
}

// No verdict at all is not readiness. Absence is not a pass.
func TestMergeReadiness_NoVerdictIsNotReady(t *testing.T) {
	l := ledgerWith(t, `{"event":"record","sha":"eee","reviewer":"r1"}`)
	got, _ := l.MergeReadinessFor("eee")
	if got.Ready {
		t.Fatalf("no verdict must not be ready: %+v", got)
	}
}

// A genuine clean pass must still be ready, so the guard cannot be satisfied by
// never returning true.
func TestMergeReadiness_CleanPassIsReady(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"record","sha":"fff","reviewer":"r1","builder_family":"anthropic","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"fff","reviewer":"r1","verdict":"PASS","builder_family":"anthropic","reviewer_family":"xai","verification_digest":"digest"}`)
	got, _ := l.MergeReadinessFor("fff")
	if !got.Ready || got.Passes != 1 {
		t.Fatalf("a clean PASS must be ready: %+v", got)
	}
}

// The ledger stores 40-char SHAs; callers hold 12-char short forms from PR head
// refs and pane names. Exact matching reported "no verdict recorded" for
// candidates that had several -- an absence that reads as safe.
func TestMergeReadiness_MatchesShortAndLongSHA(t *testing.T) {
	full := "ce46de20808a1111111111111111111111111111"
	l := ledgerWith(t, `{"event":"verdict","sha":"`+full+`","reviewer":"r1","verdict":"FAIL"}`)

	short, err := l.MergeReadinessFor("ce46de20808a")
	if err != nil {
		t.Fatal(err)
	}
	if short.Failures != 1 {
		t.Fatalf("short sha must find the verdict, got %+v", short)
	}
	if short.Ready {
		t.Fatal("a FAIL found by short sha must still block")
	}
	long, _ := l.MergeReadinessFor(full)
	if long.Failures != short.Failures {
		t.Fatalf("short and long forms must agree: %+v vs %+v", short, long)
	}
}

// A prefix shorter than 12 chars must NOT match, or unrelated commits collide.
func TestMergeReadiness_RejectsTooShortPrefix(t *testing.T) {
	l := ledgerWith(t, `{"event":"verdict","sha":"ce46de20808a1111111111111111111111111111","reviewer":"r1","verdict":"PASS"}`)
	got, _ := l.MergeReadinessFor("ce46de")
	if got.Passes != 0 {
		t.Fatalf("a 6-char prefix must not match; collision risk: %+v", got)
	}
}

// FAC-627: an honest "provenance was never recorded" must PRESERVE the review
// but must not grant the independence claim. Discarding it is what left the
// review host with 7 free lanes and ~20 candidates it was forbidden to touch.
func TestMergeReadiness_UnrecordedProvenancePassIsAdmittedButNotReady(t *testing.T) {
	l := ledgerWith(t,
		`{"event":"record","sha":"aaa1111111111111111111111111111111111111","reviewer":"r1","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"aaa1111111111111111111111111111111111111","reviewer":"r1","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","verification_digest":"digest"}`)
	got, err := l.MergeReadinessFor("aaa1111111111111111111111111111111111111")
	if err != nil {
		t.Fatal(err)
	}
	if got.Passes != 1 {
		t.Fatalf("the review must be preserved, not discarded: %+v", got)
	}
	if got.Ready {
		t.Fatalf("unprovable authorship must not read as a clean pass: %+v", got)
	}
	if got.ProvenanceUnrecorded != 1 {
		t.Fatalf("the unrecorded provenance must be counted and visible: %+v", got)
	}
}

// A candidate with BOTH an unrecorded pass and a genuine cross-family pass is
// ready: the provable review carries it.
func TestMergeReadiness_ProvableePassAlongsideUnrecordedIsReady(t *testing.T) {
	sha := "bbb1111111111111111111111111111111111111"
	l := ledgerWith(t,
		`{"event":"record","sha":"`+sha+`","reviewer":"r1","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"google","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"r1","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"google","verification_digest":"digest-1"}`,
		`{"event":"record","sha":"`+sha+`","reviewer":"r2","gate":"independent","builder_family":"anthropic","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"r2","verdict":"PASS","gate":"independent","builder_family":"anthropic","reviewer_family":"xai","verification_digest":"digest-2"}`)
	got, _ := l.MergeReadinessFor(sha)
	if !got.Ready {
		t.Fatalf("a provable PASS must still carry the candidate: %+v", got)
	}
}

// FAC-630 live shape: the verdict row was complete and said anthropic/xai,
// while the older launch record still said unrecorded. Readiness used only the
// verdict and returned ready=true; reduced landed-receipt admission used the
// record and refused. One candidate cannot have two provenance answers.
func TestMergeReadinessAndReducedAdmissionAgreeOnHistoricalRecordProvenance(t *testing.T) {
	sha := strings.Repeat("9", 40)
	l := ledgerWith(t,
		`{"event":"record","sha":"`+sha+`","reviewer":"reviewer-a","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"reviewer-a","verdict":"PASS","builder_family":"anthropic","reviewer_family":"xai","verification_digest":"2344575ebd590030e9c06cfd230e1896"}`,
	)

	ready, err := l.MergeReadinessFor(sha)
	if err != nil {
		t.Fatal(err)
	}
	admitted, admitErr := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: sha})
	if admitErr == nil || admitted == nil || admitted.Admitted {
		t.Fatalf("fixture must be refused by reduced admission: result=%+v err=%v", admitted, admitErr)
	}
	if ready.Ready != admitted.Admitted {
		t.Fatalf("readiness/admission disagree: readiness=%+v admission=%+v err=%v", ready, admitted, admitErr)
	}
	if ready.ProvenanceUnrecorded != 1 {
		t.Fatalf("readiness did not use launch-row provenance: %+v", ready)
	}
	for _, want := range []string{`builder_family="unrecorded"`, "allowlisted=false"} {
		if !strings.Contains(ready.Reason, want) {
			t.Errorf("readiness reason %q does not contain %q", ready.Reason, want)
		}
	}
}

// FAC-667 landed symptom: a literal Verification section can still produce a
// durable PASS row with no digest. Readiness must not invent one or claim ready
// when the exact admission predicate will refuse it.
func TestMergeReadinessRefusesPassWithoutVerificationDigest(t *testing.T) {
	sha := strings.Repeat("8", 40)
	l := ledgerWith(t,
		`{"event":"record","sha":"`+sha+`","reviewer":"reviewer-a","builder_family":"anthropic","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"reviewer-a","verdict":"PASS","builder_family":"anthropic","reviewer_family":"xai"}`,
	)

	got, err := l.MergeReadinessFor(sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready {
		t.Fatalf("a PASS with no verification digest must not be ready: %+v", got)
	}
	if !strings.Contains(got.Reason, `verification_digest=""`) {
		t.Fatalf("missing digest refusal was not named exactly: %q", got.Reason)
	}
}

func TestMergeReadinessAndReducedAdmissionShareThePassPredicate(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]any
	}{
		{name: "clean", rows: []map[string]any{record("anthropic", map[string]any{"tier": "R3"}), verdictRow(nil)}},
		{name: "digest", rows: []map[string]any{record("anthropic", map[string]any{"tier": "R3"}), verdictRow(map[string]any{"verification_digest": ""})}},
		{name: "tier", rows: []map[string]any{record("anthropic", nil), verdictRow(nil)}},
		{name: "provenance", rows: []map[string]any{record(FamilyUnrecorded, map[string]any{"gate": GateProvenanceUnrecorded, "tier": "R3"}), verdictRow(nil)}},
		{name: "family independence", rows: []map[string]any{record("anthropic", map[string]any{"reviewer_family": "anthropic", "tier": "R3"}), verdictRow(map[string]any{"reviewer_family": "anthropic"})}},
		{name: "identity independence", rows: []map[string]any{record("anthropic", map[string]any{"builder_identity": "rv", "tier": "R3"}), verdictRow(nil)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := diagLedger(t, tc.rows...)
			readiness, readinessErr := l.MergeReadinessFor(diagSHA)
			if readinessErr != nil {
				t.Fatal(readinessErr)
			}
			admission, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
			if admission == nil {
				t.Fatal("no reduced-admission result")
			}
			if readiness.Ready != admission.Admitted {
				t.Fatalf("same evidence produced different decisions: readiness=%+v admission=%+v", readiness, admission)
			}
		})
	}
}

func TestMergeReadinessAndReducedAdmissionBothRefuseAConsumedCandidate(t *testing.T) {
	l := diagLedger(t, record("anthropic", map[string]any{"tier": "R3"}), verdictRow(nil))
	if err := os.WriteFile(l.QueuePath, []byte(`{"event":"consumed","sha":"`+diagSHA+`"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	readiness, err := l.MergeReadinessFor(diagSHA)
	if err != nil {
		t.Fatal(err)
	}
	admission, _ := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: diagSHA})
	if readiness.Ready || admission == nil || admission.Admitted {
		t.Fatalf("consumed evidence did not refuse both surfaces: readiness=%+v admission=%+v", readiness, admission)
	}
	if !strings.Contains(readiness.Reason, "admission_condition=consumption") {
		t.Fatalf("readiness did not name the consumed gate: %q", readiness.Reason)
	}
}

func TestMergeReadinessNamesAnUnreadableAdmissionQueue(t *testing.T) {
	l := diagLedger(t, record("anthropic", map[string]any{"tier": "R3"}), verdictRow(nil))
	if err := os.Mkdir(l.QueuePath, 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := l.MergeReadinessFor(diagSHA)
	if err == nil || got.Ready {
		t.Fatalf("unreadable queue did not fail closed: readiness=%+v err=%v", got, err)
	}
	if !strings.Contains(got.Reason, "admission_condition=admission_queue observed queue_readable=false") {
		t.Fatalf("readiness misnamed the unreadable evidence source: %q", got.Reason)
	}
}

// A FAIL under the unrecorded gate still blocks -- admitting the review does not
// weaken a negative verdict.
func TestMergeReadiness_UnrecordedFailStillBlocks(t *testing.T) {
	sha := "ccc1111111111111111111111111111111111111"
	l := ledgerWith(t,
		`{"event":"record","sha":"`+sha+`","reviewer":"r1","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"r1","verdict":"FAIL","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai"}`)
	got, _ := l.MergeReadinessFor(sha)
	if got.Ready {
		t.Fatalf("a FAIL must block regardless of provenance: %+v", got)
	}
}

// The unrecorded gate must reject a real family: it exists for the honest
// unknown case only, not as a bypass for recording a family without proof.
func TestValidateRecord_UnrecordedGateRejectsARealFamily(t *testing.T) {
	err := validateRecord(RecordOpts{Gate: GateProvenanceUnrecorded, BuilderFamily: "xai"})
	if err == nil {
		t.Fatal("the unrecorded gate must not accept an asserted family; that is the bypass it exists to avoid")
	}
}

func TestValidateRecord_UnrecordedGateAcceptsUnrecorded(t *testing.T) {
	if err := validateRecord(RecordOpts{Gate: GateProvenanceUnrecorded, BuilderFamily: FamilyUnrecorded}); err != nil {
		t.Fatalf("honest unrecorded provenance must be admissible: %v", err)
	}
}

// FAC-641: an EMPTY ledger is not "nothing has been reviewed". The coordinator
// runs from a worktree whose ledger is a 0-byte file while the shared one holds
// 1968 rows; reading the empty one reported all 71 open heads as no-verdict,
// which would have dispatched 71 redundant reviews and hidden 4 ready candidates.
func TestMergeReadiness_EmptyLedgerFailsClosed(t *testing.T) {
	l := ledgerWith(t) // no rows at all
	got, err := l.MergeReadinessFor(strings.Repeat("a", 40))
	if err == nil {
		t.Fatal("an empty ledger must be an error, not a silent no-verdict")
	}
	if got.Ready {
		t.Fatal("an empty ledger must never report ready")
	}
	if !strings.Contains(got.Reason, "EMPTY") {
		t.Errorf("the reason must name the empty ledger so the caller can tell it is pointed at the wrong file: %q", got.Reason)
	}
}

// A populated ledger with no verdict for THIS sha is a genuine no-verdict and
// must still report normally, so the guard cannot be satisfied by always erroring.
func TestMergeReadiness_PopulatedLedgerStillReportsGenuineNoVerdict(t *testing.T) {
	l := ledgerWith(t, `{"event":"verdict","sha":"`+strings.Repeat("b", 40)+`","reviewer":"r1","verdict":"PASS"}`)
	got, err := l.MergeReadinessFor(strings.Repeat("a", 40))
	if err != nil {
		t.Fatalf("a populated ledger must not error for an unknown sha: %v", err)
	}
	if got.Ready {
		t.Fatal("an unknown sha must not be ready")
	}
	if !strings.Contains(got.Reason, "no verdict recorded") {
		t.Errorf("a genuine no-verdict must still be reportable: %q", got.Reason)
	}
}

// FAC-641: a lane in its own worktree must be able to address the authoritative
// ledger without cd-ing into the shared checkout, because coordinator residency
// in a private worktree is the correct arrangement and must not be traded away
// for read access.
func TestDefaultPath_HonoursExplicitLedgerOverride(t *testing.T) {
	t.Setenv("HERD_REVIEW_LEDGER", "/shared/.herd/review-ledger.jsonl")
	if got := DefaultPath("/some/private/worktree"); got != "/shared/.herd/review-ledger.jsonl" {
		t.Fatalf("override must win over cwd-derived path, got %q", got)
	}
	t.Setenv("HERD_REVIEW_LEDGER", "")
	if got := DefaultPath("/some/private/worktree"); got == "/shared/.herd/review-ledger.jsonl" {
		t.Fatal("a blank override must fall through to the root-derived path")
	}
}

// FAC-668: "provenance was never recorded" was a permanent verdict on work that
// had a real PASS. On a fleet where commits are authored under one shared human
// identity with no trailers, the builder family is genuinely unknowable after
// the fact -- so seven candidates sat blocked forever and a reviewer reporting
// "cannot merge, provenance not set" was correct with nowhere to go. A gate
// nobody can satisfy is not a safety property, it is an outage.
func TestUnrecordedProvenanceIsOperatorDecidableNotADeadEnd(t *testing.T) {
	sha := strings.Repeat("f", 40)
	l := ledgerWith(t,
		`{"event":"record","sha":"`+sha+`","reviewer":"pool-06","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"pool-06","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","verification_digest":"digest"}`)

	got, err := l.MergeReadinessFor(sha)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ready {
		t.Fatal("the default posture must stay fail-closed")
	}
	if !got.OperatorDecidable {
		t.Error("a candidate blocked ONLY by unrecorded provenance must be marked decidable, not merely failed")
	}
	// The refusal must say what would unblock it. A dead end that does not name
	// its exit is what left the reviewer stuck.
	for _, want := range []string{"--allow-unrecorded-provenance", "herd launch-record"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("the refusal must name the path forward %q: %q", want, got.Reason)
		}
	}
}

// Accepting it is explicit and AUDITABLE: the result records that the choice was
// made, and still refuses to claim the independence it cannot prove.
func TestAcceptingUnrecordedProvenanceIsRecordedOnTheResult(t *testing.T) {
	sha := strings.Repeat("f", 40)
	l := ledgerWith(t,
		`{"event":"record","sha":"`+sha+`","reviewer":"pool-06","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","tier":"R3"}`,
		`{"event":"verdict","sha":"`+sha+`","reviewer":"pool-06","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","verification_digest":"digest"}`)

	got, err := l.MergeReadinessAllowingUnrecordedProvenance(sha)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Ready {
		t.Fatal("explicit acceptance must make it mergeable")
	}
	if !got.AdmittedWithoutProvenance {
		t.Error("the choice must be recorded on the result, not implicit")
	}
	if !strings.Contains(got.Reason, "NOT claimed") {
		t.Errorf("it must still refuse to claim independence it cannot prove: %q", got.Reason)
	}
}

// The escape hatch must NOT rescue a genuine failure. A FAIL stays a FAIL even
// with the flag, or this would be a bypass rather than a decision.
func TestAcceptingUnrecordedProvenanceNeverRescuesAFailure(t *testing.T) {
	sha := strings.Repeat("f", 40)
	for _, verdict := range []string{"FAIL", "BLOCKED"} {
		l := ledgerWith(t,
			`{"event":"verdict","sha":"`+sha+`","reviewer":"r1","verdict":"`+verdict+`","gate":"provenance-unrecorded","builder_family":"unrecorded"}`)
		got, err := l.MergeReadinessAllowingUnrecordedProvenance(sha)
		if err != nil {
			t.Fatal(err)
		}
		if got.Ready {
			t.Fatalf("%s must never become ready; the flag accepts unknown PROVENANCE, not dissent", verdict)
		}
		if got.OperatorDecidable {
			t.Errorf("%s is not operator-decidable: something other than provenance is wrong", verdict)
		}
	}
}

func TestAcceptingUnrecordedProvenanceNeverRescuesMissingDurableEvidence(t *testing.T) {
	sha := strings.Repeat("e", 40)
	for _, tc := range []struct {
		name       string
		tier       string
		digest     string
		wantReason string
	}{
		{name: "verification digest", tier: "R3", wantReason: `verification_digest=""`},
		{name: "risk tier", digest: "digest", wantReason: `risk_tier=""`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := ledgerWith(t,
				`{"event":"record","sha":"`+sha+`","reviewer":"r1","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","tier":"`+tc.tier+`"}`,
				`{"event":"verdict","sha":"`+sha+`","reviewer":"r1","verdict":"PASS","gate":"provenance-unrecorded","builder_family":"unrecorded","reviewer_family":"xai","verification_digest":"`+tc.digest+`"}`)
			got, err := l.MergeReadinessAllowingUnrecordedProvenance(sha)
			if err != nil {
				t.Fatal(err)
			}
			if got.Ready || got.OperatorDecidable || got.AdmittedWithoutProvenance {
				t.Fatalf("provenance override rescued missing %s: %+v", tc.name, got)
			}
			if !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason %q does not name missing %s as %q", got.Reason, tc.name, tc.wantReason)
			}
		})
	}
}
