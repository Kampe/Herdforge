package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestVerdictProjectionsShareRetryAuthority(t *testing.T) {
	const current = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const previous = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	type want struct {
		vetoed   bool
		vetoSHAs bool
		passSHAs bool
	}
	cases := []struct {
		name  string
		sha   string
		setup func(*testing.T, *reviewledger.Ledger)
		want  want
	}{
		{
			name: "same_host_retry_clears_named_veto",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				recordBoth(t, l, current, "reviewer-a", "host-a", "reviewer-b", "host-a")
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "")
				mustVerdict(t, l, current, "reviewer-b", "host-a", reviewledger.VerdictPASS, "reviewer-a")
			},
			want: want{vetoed: false, vetoSHAs: false, passSHAs: true},
		},
		{
			name: "cross_host_fail_fail_then_pass",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				recordBoth(t, l, current, "reviewer-a", "host-a", "reviewer-a", "host-b")
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "")
				mustVerdict(t, l, current, "reviewer-a", "host-b", reviewledger.VerdictPASS, "reviewer-a")
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
		{
			name: "cross_host_fail_pass_then_fail",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				recordBoth(t, l, current, "reviewer-a", "host-a", "reviewer-a", "host-b")
				mustVerdict(t, l, current, "reviewer-a", "host-b", reviewledger.VerdictPASS, "reviewer-a")
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "")
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
		{
			name: "cross_host_blocked_fail_then_pass",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				recordBoth(t, l, current, "reviewer-a", "host-a", "reviewer-a", "host-b")
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictBLOCKED, "")
				mustVerdict(t, l, current, "reviewer-a", "host-b", reviewledger.VerdictPASS, "reviewer-a")
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
		{
			name: "cross_host_blocked_pass_then_fail",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				recordBoth(t, l, current, "reviewer-a", "host-a", "reviewer-a", "host-b")
				mustVerdict(t, l, current, "reviewer-a", "host-b", reviewledger.VerdictPASS, "reviewer-a")
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictBLOCKED, "")
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
		{
			name: "generic_identity_supersession_does_not_clear",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				recordBoth(t, l, current, "reviewer-a", "host-a", "reviewer-b", "host-a")
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "")
				mustVerdict(t, l, current, "reviewer-b", "host-a", reviewledger.VerdictPASS, "")
				if err := l.Supersession(reviewledger.DecisionOpts{
					SHA: current, PreviousSHA: previous, Reviewer: "reviewer-a",
					Reason: "candidate identity replacement",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
		{
			name: "event_revoked_does_not_clear_fail",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				if err := l.Record(reviewledger.RecordOpts{
					SHA: current, Reviewer: "reviewer-a", Host: "host-a",
					BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent",
				}); err != nil {
					t.Fatal(err)
				}
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "")
				appendMainRow(t, l, map[string]string{
					"event": string(reviewledger.EventRevoked), "sha": current, "host": "host-a",
				})
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
		{
			name: "identity_replacement_leaves_previous_fail",
			sha:  current,
			setup: func(t *testing.T, l *reviewledger.Ledger) {
				if err := l.Record(reviewledger.RecordOpts{
					SHA: current, Reviewer: "reviewer-a", Host: "host-a",
					BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent",
				}); err != nil {
					t.Fatal(err)
				}
				mustVerdict(t, l, current, "reviewer-a", "host-a", reviewledger.VerdictFAIL, "")
				if err := l.Supersession(reviewledger.DecisionOpts{
					SHA: previous, PreviousSHA: current, Reviewer: "reviewer-a",
					Reason: "candidate identity replacement",
				}); err != nil {
					t.Fatal(err)
				}
			},
			want: want{vetoed: true, vetoSHAs: true, passSHAs: false},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "review-ledger.jsonl")
			rl, err := reviewledger.NewReviewLedger(dir, path)
			if err != nil {
				t.Fatal(err)
			}
			tc.setup(t, rl)
			l := OpenLedger(path)
			assertVerdictProjections(t, l, tc.sha, tc.want.vetoed, tc.want.vetoSHAs, tc.want.passSHAs)
		})
	}
}

func recordBoth(t *testing.T, l *reviewledger.Ledger, sha, a, hostA, b, hostB string) {
	t.Helper()
	for _, rec := range []reviewledger.RecordOpts{
		{SHA: sha, Reviewer: a, Host: hostA, BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"},
		{SHA: sha, Reviewer: b, Host: hostB, BuilderFamily: "anthropic", ReviewerFamily: "openai", Gate: "independent"},
	} {
		if err := l.Record(rec); err != nil {
			t.Fatal(err)
		}
	}
}

func mustVerdict(t *testing.T, l *reviewledger.Ledger, sha, reviewer, host string, v reviewledger.Verdict, retryOf string) {
	t.Helper()
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: reviewer, Host: host, Verdict: v,
		ReviewerFamily: "openai", BuilderFamily: "anthropic", RetryOf: retryOf,
	}); err != nil {
		t.Fatal(err)
	}
}

func appendMainRow(t *testing.T, l *reviewledger.Ledger, row map[string]string) {
	t.Helper()
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(f).Encode(row); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertVerdictProjections(t *testing.T, l *Ledger, sha string, wantVetoed, wantVetoSHAs, wantPassSHAs bool) {
	t.Helper()
	veto, err := l.Vetoed(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got := veto[sha]; got != wantVetoed {
		t.Fatalf("Vetoed[%s]=%v want %v map=%v", sha, got, wantVetoed, veto)
	}
	snap, err := l.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if got := snap.Vetoed()[sha]; got != wantVetoed {
		t.Fatalf("Snapshot.Vetoed[%s]=%v want %v", sha, got, wantVetoed)
	}
	listed, err := l.VetoSHAs()
	if err != nil {
		t.Fatal(err)
	}
	if got := containsSHA(listed, sha); got != wantVetoSHAs {
		t.Fatalf("VetoSHAs %v contains %s=%v want %v", listed, sha, got, wantVetoSHAs)
	}
	passes, err := l.PassSHAs()
	if err != nil {
		t.Fatal(err)
	}
	if got := containsSHA(passes, sha); got != wantPassSHAs {
		t.Fatalf("PassSHAs %v contains %s=%v want %v", passes, sha, got, wantPassSHAs)
	}
}
