package reviewledger

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func completionFixture(t *testing.T, mutate func(*LedgerRow, *LedgerRow)) (*Ledger, string, string) {
	t.Helper()
	l := newTestLedger(t)
	sha := strings.Repeat("a", 40)
	reviewer := "independent"
	r := LedgerRow{Event: string(EventRecord), SHA: sha, Reviewer: reviewer, Task: "FAC-759", BuilderFamily: FamilyUnrecorded, Gate: GateProvenanceUnrecorded, Lease: "real-lease", PatchURL: "real-patch"}
	v := LedgerRow{Event: string(EventVerdict), SHA: sha, CandidateSHA: sha, Reviewer: reviewer, Task: "FAC-759", BuilderFamily: "openai", ReviewerFamily: "google", Verdict: string(VerdictPASS), VerificationDigest: "real-verification", ArtifactDigest: strings.Repeat("b", 64)}
	if mutate != nil {
		mutate(&r, &v)
	}
	if e := l.appendRow(l.Path, &r); e != nil {
		t.Fatal(e)
	}
	if e := l.appendRow(l.Path, &v); e != nil {
		t.Fatal(e)
	}
	return l, sha, reviewer
}

func TestCompleteAdmissionRecordReconcilesLegacyBranchTask(t *testing.T) {
	l, sha, reviewer := completionFixture(t, func(r, _ *LedgerRow) {
		r.Task = "recovery/fac-655-record-completion"
	})
	before, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) {
		return RecordCompletion{Branch: "recovery/fac-655-record-completion", Tier: "R3"}, nil
	}); err != nil {
		t.Fatalf("legacy branch placeholder: %v", err)
	}
	after, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(after), string(before)) {
		t.Fatal("rewrote history")
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	last := rows[len(rows)-1]
	if last.Event != string(EventRecord) || last.Task != "FAC-759" || last.BuilderFamily != "openai" || last.ReviewerFamily != "google" || last.Tier != "R3" || last.Branch != "recovery/fac-655-record-completion" || last.Gate != "independent" || last.Lease != "real-lease" || last.PatchURL != "real-patch" {
		t.Fatalf("incomplete recovered record: %+v", last)
	}
	if err := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) {
		return RecordCompletion{Branch: "recovery/fac-655-record-completion", Tier: "R3"}, nil
	}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
	repeat, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(repeat) != len(rows) {
		t.Fatalf("repeat appended %d rows, want %d", len(repeat), len(rows))
	}
}

func TestCompleteAdmissionRecordRefusesConflicts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*LedgerRow, *LedgerRow)
	}{
		{"record task", func(r, v *LedgerRow) { r.Task = "FAC-758" }},
		{"legacy branch mismatch", func(r, v *LedgerRow) { r.Task = "other-branch" }},
		{"verdict task", func(r, v *LedgerRow) { v.Task = "FAC-758" }},
		{"candidate", func(r, v *LedgerRow) { v.CandidateSHA = strings.Repeat("c", 40) }},
		{"FAIL", func(r, v *LedgerRow) { v.Verdict = string(VerdictFAIL) }},
		{"missing digest", func(r, v *LedgerRow) { v.VerificationDigest = "" }},
		{"missing artifact", func(r, v *LedgerRow) { v.ArtifactDigest = "" }},
		{"same family", func(r, v *LedgerRow) { v.ReviewerFamily = v.BuilderFamily }},
		{"unknown family", func(r, v *LedgerRow) { v.BuilderFamily = FamilyUnrecorded }},
		{"changed builder", func(r, v *LedgerRow) { r.BuilderFamily = "anthropic" }},
		{"changed reviewer", func(r, v *LedgerRow) { r.ReviewerFamily = "anthropic" }},
		{"changed branch", func(r, v *LedgerRow) { r.Branch = "different" }},
		{"changed tier", func(r, v *LedgerRow) { r.Tier = "R0" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, sha, reviewer := completionFixture(t, tc.mutate)
			before, e := os.ReadFile(l.Path)
			if e != nil {
				t.Fatal(e)
			}
			e = l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) { return RecordCompletion{Branch: "work", Tier: "R3"}, nil })
			if e == nil {
				t.Fatal("conflicting evidence admitted")
			}
			after, e := os.ReadFile(l.Path)
			if e != nil {
				t.Fatal(e)
			}
			if string(after) != string(before) {
				t.Fatal("refusal changed ledger")
			}
		})
	}
}

func TestCompleteAdmissionRecordRejectsDissentAndUnverifiedEvidence(t *testing.T) {
	for _, dissent := range []bool{false, true} {
		t.Run(fmt.Sprint(dissent), func(t *testing.T) {
			l, sha, reviewer := completionFixture(t, nil)
			if dissent {
				if e := l.appendRow(l.Path, &LedgerRow{Event: string(EventVerdict), SHA: sha, Reviewer: "other-reviewer", Task: "FAC-759", Verdict: string(VerdictBLOCKED)}); e != nil {
					t.Fatal(e)
				}
			}
			e := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) {
				if !dissent {
					return RecordCompletion{}, fmt.Errorf("native proof missing")
				}
				return RecordCompletion{Branch: "work", Tier: "R3"}, nil
			})
			if e == nil {
				t.Fatal("unverified or disputed evidence admitted")
			}
		})
	}
}

func TestCompleteAdmissionRecordSerializesAliases(t *testing.T) {
	l, sha, reviewer := completionFixture(t, nil)
	alias := filepath.Join(l.RepoRoot, "alias.jsonl")
	if e := os.Symlink(l.Path, alias); e != nil {
		t.Fatal(e)
	}
	other, e := NewReadOnlyReviewLedger(l.RepoRoot, alias)
	if e != nil {
		t.Fatal(e)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	one := make(chan error, 1)
	two := make(chan error, 1)
	go func() {
		one <- l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) {
			close(entered)
			<-release
			return RecordCompletion{Branch: "work", Tier: "R3"}, nil
		})
	}()
	<-entered
	go func() {
		two <- other.CompleteAdmissionRecord("FAC-759", sha, reviewer, "", func(LedgerRow) (RecordCompletion, error) { return RecordCompletion{Branch: "work", Tier: "R3"}, nil })
	}()
	select {
	case e := <-two:
		t.Fatalf("second completion escaped locked evidence check: %v", e)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if e := <-one; e != nil {
		t.Fatal(e)
	}
	if e := <-two; e != nil {
		t.Fatal(e)
	}
	rows, e := l.AllRows()
	if e != nil {
		t.Fatal(e)
	}
	if len(rows) != 3 {
		t.Fatalf("want one appended record, got %d rows", len(rows))
	}
}

func TestCompletionRequiresExactHostProjection(t *testing.T) {
	orders := [][]string{{"host-b", "host-a"}, {"host-a", "host-b"}}
	for _, order := range orders {
		t.Run(strings.Join(order, "_then_"), func(t *testing.T) {
			l := newTestLedger(t)
			sha := strings.Repeat("a", 40)
			reviewer := "independent"
			for _, host := range order {
				r := LedgerRow{Event: string(EventRecord), SHA: sha, Reviewer: reviewer, Host: host, Task: "FAC-759", BuilderFamily: FamilyUnrecorded, Gate: GateProvenanceUnrecorded, Lease: "real-lease", PatchURL: "real-patch"}
				if e := l.appendRow(l.Path, &r); e != nil {
					t.Fatal(e)
				}
			}
			v := LedgerRow{Event: string(EventVerdict), SHA: sha, CandidateSHA: sha, Reviewer: reviewer, Host: "host-b", Task: "FAC-759", BuilderFamily: "openai", ReviewerFamily: "google", Verdict: string(VerdictPASS), VerificationDigest: "real-verification", ArtifactDigest: strings.Repeat("b", 64)}
			if e := l.appendRow(l.Path, &v); e != nil {
				t.Fatal(e)
			}
			verify := func(got LedgerRow) (RecordCompletion, error) {
				if got.Host != "host-b" {
					t.Fatalf("completion used host %q", got.Host)
				}
				return RecordCompletion{Branch: "work", Tier: "R3"}, nil
			}
			if e := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "host-a", verify); e == nil {
				t.Fatal("host-B PASS completed host-A record")
			}
			if e := l.CompleteAdmissionRecord("FAC-759", sha, reviewer, "host-b", verify); e != nil {
				t.Fatalf("host-B completion: %v", e)
			}
		})
	}
}

func effectiveLaunchRecord(t *testing.T, l *Ledger, sha, reviewer, host string) LedgerRow {
	t.Helper()
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	want := ProjectionOf(sha, reviewer, host)
	var last *LedgerRow
	for i := range rows {
		if rows[i].Event == string(EventRecord) && rowProjection(rows[i]) == want {
			copy := rows[i]
			last = &copy
		}
	}
	if last == nil {
		t.Fatalf("no launch record for sha=%s reviewer=%s host=%q", sha, reviewer, host)
	}
	return *last
}

func seedBranchPlaceholder(t *testing.T, l *Ledger, sha, reviewer, host, lease, patch, pane string) {
	t.Helper()
	if err := l.Record(RecordOpts{
		SHA: sha, Reviewer: reviewer, Host: host,
		Task:          "recovery/fac-655-record-completion",
		BuilderFamily: FamilyUnrecorded, Gate: GateProvenanceUnrecorded,
		Lease: lease, PatchURL: patch, Pane: pane,
	}); err != nil {
		t.Fatal(err)
	}
}

func authenticatedHostIngest(sha, reviewer, host string) IngestOpts {
	branch := "recovery/fac-655-record-completion"
	return IngestOpts{
		Record: RecordOpts{
			SHA: sha, Branch: branch, BuilderFamily: "xai", ReviewerFamily: "anthropic",
			Reviewer: reviewer, Host: host, Artifact: ".herd/review/inbox/retained.md",
			Gate: "independent", Tier: "R3", Task: "FAC-655",
		},
		Verdict: VerdictOpts{
			SHA: sha, Reviewer: reviewer, Host: host, Verdict: VerdictPASS,
			ReviewerFamily: "anthropic", BuilderFamily: "xai", Branch: branch,
			Artifact: ".herd/review/inbox/retained.md", Task: "FAC-655",
			VfyDigest: "vfy", CandidateSHA: sha,
		},
	}
}

func assertQueuedEligibleReady(t *testing.T, l *Ledger, sha, host string) {
	t.Helper()
	queued, err := l.Queued()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, q := range queued {
		if q.SHA == sha && hostKey(q.Host) == hostKey(host) {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("Queued omitted sha=%s host=%q: %+v", sha, host, queued)
	}
	eligible, err := l.Eligible(sha, "xai")
	if err != nil || !eligible {
		t.Fatalf("Eligible: eligible=%v err=%v", eligible, err)
	}
	ready, rerr := l.MergeReadinessFor(sha)
	if rerr != nil || !ready.Ready {
		t.Fatalf("MergeReadiness: ready=%+v err=%v", ready, rerr)
	}
}

func TestIngestCompletesExactHostWithoutMutatingPeerHost(t *testing.T) {
	orders := [][]string{{"host-b", "host-a"}, {"host-a", "host-b"}}
	for _, order := range orders {
		t.Run(strings.Join(order, "_then_"), func(t *testing.T) {
			l := newTestLedger(t)
			sha := strings.Repeat("c", 40)
			reviewer := "independent-reviewer"
			meta := map[string]struct{ lease, patch, pane string }{
				"host-a": {"lease-a", "patch-a", "pane-a"},
				"host-b": {"lease-b", "patch-b", "pane-b"},
			}
			for _, host := range order {
				p := meta[host]
				seedBranchPlaceholder(t, l, sha, reviewer, host, p.lease, p.patch, p.pane)
			}
			before, err := os.ReadFile(l.Path)
			if err != nil {
				t.Fatal(err)
			}
			hostABefore := effectiveLaunchRecord(t, l, sha, reviewer, "host-a")
			opts := authenticatedHostIngest(sha, reviewer, "host-b")
			enqueued, err := l.Ingest(opts)
			if err != nil {
				t.Fatalf("ingest host-b: %v", err)
			}
			if !enqueued {
				t.Fatal("host-B PASS was not queued")
			}
			after, err := os.ReadFile(l.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(after), string(before)) {
				t.Fatal("rewrote history")
			}
			hostA := effectiveLaunchRecord(t, l, sha, reviewer, "host-a")
			if hostA.Host != "host-a" || hostA.BuilderFamily != FamilyUnrecorded || hostA.ReviewerFamily != hostABefore.ReviewerFamily || hostA.Tier != hostABefore.Tier || hostA.Gate != GateProvenanceUnrecorded || hostA.Lease != "lease-a" || hostA.PatchURL != "patch-a" || hostA.Pane != "pane-a" || hostA.Task != hostABefore.Task {
				t.Fatalf("host-A mutated: before=%+v after=%+v", hostABefore, hostA)
			}
			hostB := effectiveLaunchRecord(t, l, sha, reviewer, "host-b")
			if hostB.Host != "host-b" || hostB.Task != "FAC-655" || hostB.BuilderFamily != "xai" || hostB.ReviewerFamily != "anthropic" || hostB.Tier != "R3" || hostB.Gate != "independent" || hostB.Lease != "lease-b" || hostB.PatchURL != "patch-b" || hostB.Pane != "pane-b" {
				t.Fatalf("host-B not completed in place: %+v", hostB)
			}
			assertQueuedEligibleReady(t, l, sha, "host-b")
			enqueued, err = l.Ingest(opts)
			if err != nil {
				t.Fatalf("duplicate ingest: %v", err)
			}
			if enqueued {
				t.Fatal("duplicate ingest re-queued")
			}
		})
	}
}

func TestIngestEmptyHostCompletesOnlyUnhostedPlaceholder(t *testing.T) {
	orders := [][]string{{"host-a", ""}, {"", "host-a"}}
	for _, order := range orders {
		name := "host-a_then_unhosted"
		if order[0] == "" {
			name = "unhosted_then_host-a"
		}
		t.Run(name, func(t *testing.T) {
			l := newTestLedger(t)
			sha := strings.Repeat("d", 40)
			reviewer := "independent-reviewer"
			for _, host := range order {
				lease, patch, pane := "lease-hosted", "patch-hosted", "pane-hosted"
				if host == "" {
					lease, patch, pane = "lease-unhosted", "patch-unhosted", "pane-unhosted"
				}
				seedBranchPlaceholder(t, l, sha, reviewer, host, lease, patch, pane)
			}
			before, err := os.ReadFile(l.Path)
			if err != nil {
				t.Fatal(err)
			}
			hostedBefore := effectiveLaunchRecord(t, l, sha, reviewer, "host-a")
			opts := authenticatedHostIngest(sha, reviewer, "")
			enqueued, err := l.Ingest(opts)
			if err != nil {
				t.Fatalf("ingest unhosted: %v", err)
			}
			if !enqueued {
				t.Fatal("unhosted PASS was not queued")
			}
			after, err := os.ReadFile(l.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(string(after), string(before)) {
				t.Fatal("rewrote history")
			}
			hosted := effectiveLaunchRecord(t, l, sha, reviewer, "host-a")
			if hosted.Host != "host-a" || hosted.BuilderFamily != FamilyUnrecorded || hosted.Tier != hostedBefore.Tier || hosted.Gate != GateProvenanceUnrecorded || hosted.Lease != "lease-hosted" || hosted.PatchURL != "patch-hosted" || hosted.Pane != "pane-hosted" || hosted.Task != hostedBefore.Task {
				t.Fatalf("empty-host ingest completed hosted row: before=%+v after=%+v", hostedBefore, hosted)
			}
			unhosted := effectiveLaunchRecord(t, l, sha, reviewer, "")
			if unhosted.Host != "" || unhosted.Task != "FAC-655" || unhosted.BuilderFamily != "xai" || unhosted.ReviewerFamily != "anthropic" || unhosted.Tier != "R3" || unhosted.Gate != "independent" || unhosted.Lease != "lease-unhosted" || unhosted.PatchURL != "patch-unhosted" || unhosted.Pane != "pane-unhosted" {
				t.Fatalf("unhosted placeholder not completed: %+v", unhosted)
			}
			assertQueuedEligibleReady(t, l, sha, "")
		})
	}
}

func TestIngestWrongHostDoesNotCompletePeerPlaceholder(t *testing.T) {
	l := newTestLedger(t)
	sha := strings.Repeat("e", 40)
	reviewer := "independent-reviewer"
	seedBranchPlaceholder(t, l, sha, reviewer, "host-a", "lease-a", "patch-a", "pane-a")
	seedBranchPlaceholder(t, l, sha, reviewer, "host-b", "lease-b", "patch-b", "pane-b")
	if _, err := l.Ingest(authenticatedHostIngest(sha, reviewer, "host-a")); err != nil {
		t.Fatalf("ingest host-a: %v", err)
	}
	hostB := effectiveLaunchRecord(t, l, sha, reviewer, "host-b")
	if hostB.BuilderFamily != FamilyUnrecorded || hostB.Gate != GateProvenanceUnrecorded || hostB.Lease != "lease-b" || hostB.Task != "recovery/fac-655-record-completion" {
		t.Fatalf("wrong-host ingest completed host-B: %+v", hostB)
	}
	hostA := effectiveLaunchRecord(t, l, sha, reviewer, "host-a")
	if hostA.Task != "FAC-655" || hostA.BuilderFamily != "xai" || hostA.Lease != "lease-a" {
		t.Fatalf("exact host-A ingest did not complete host-A: %+v", hostA)
	}
}
