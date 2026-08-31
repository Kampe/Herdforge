package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/committime"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// FAC-659: the ordinary pool path already proves the builder family from a
// genuine launch receipt before rendering the packet. Completion must carry
// that same proof into the append-only ledger instead of replacing it with the
// unrecorded sentinel.
func TestCompleteReviewLaunchProvenancePreservesReceiptBackedFamily(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	t.Setenv("HERD_REVIEW_LEDGER", "")
	root := t.TempDir()
	fac659Git(t, root, "init", "-q", "-b", "main")
	fac659Git(t, root, "config", "user.email", "fac659@test.invalid")
	fac659Git(t, root, "config", "user.name", "fac659")
	fac659Git(t, root, "commit", "-q", "--allow-empty", "-m", "base")
	fac659Git(t, root, "branch", "origin/main")
	fac659Git(t, root, "checkout", "-q", "-b", "herd/fac-659")
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("FAC-659\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fac659Git(t, root, "add", "candidate.txt")
	fac659Git(t, root, "commit", "-q", "-m", "candidate")
	sha := fac659Git(t, root, "rev-parse", "HEAD")

	receiptPath := launch.ReceiptPathFor(root)
	receipt := launch.Receipt{
		CreatedAt:     committime.Of(root, sha).Add(-time.Minute),
		TaskRef:       "FAC-659",
		Provider:      "claude",
		Model:         "claude-sonnet-5",
		BuilderFamily: "anthropic",
		Branch:        "herd/fac-659",
		Accepted:      true,
	}
	if err := (&launch.JSONLSink{Path: receiptPath}).Write(receipt); err != nil {
		t.Fatalf("write launch receipt: %v", err)
	}

	provenFamily, err := provenBuilderFamily(root, sha)
	if err != nil {
		t.Fatalf("resolve builder provenance: %v", err)
	}
	if err := completeReviewLaunchProvenance(root, "FAC-659", sha, "pool-01-7", provenFamily); err != nil {
		t.Fatalf("complete review launch provenance: %v", err)
	}
	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, reviewledger.PathFor(root))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := ledger.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	var got *reviewledger.LedgerRow
	for i := range rows {
		if rows[i].Event == string(reviewledger.EventRecord) && rows[i].SHA == sha {
			got = &rows[i]
		}
	}
	if got == nil {
		t.Fatalf("no launch record for %s: %+v", sha, rows)
	}
	if got.BuilderFamily != "anthropic" {
		t.Fatalf("builder_family = %q, want receipt-proven anthropic; packet and ledger disagree: %+v", got.BuilderFamily, got)
	}
	if got.Gate == reviewledger.GateProvenanceUnrecorded {
		t.Fatalf("gate = %q; a genuine receipt reaching the candidate must remain independent", got.Gate)
	}
}

func TestCompleteReviewLaunchProvenanceUpgradesUnrecordedIdempotently(t *testing.T) {
	root, sha, commitAt := fac659Candidate(t)
	receipt := launch.Receipt{
		CreatedAt: commitAt.Add(-time.Minute), TaskRef: "FAC-659", Role: launch.WorkerRole,
		Provider: "claude", Model: "claude-sonnet-5", BuilderFamily: "anthropic",
		Branch: "herd/fac-659", Accepted: true,
	}
	if err := (&launch.JSONLSink{Path: launch.ReceiptPathFor(root)}).Write(receipt); err != nil {
		t.Fatal(err)
	}
	reviewer := reviewAgentName("FAC-659", sha)
	ledger, err := reviewledger.NewReviewLedger(root, reviewledger.PathFor(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.EnsureRecord(reviewledger.RecordOpts{
		SHA: sha, Reviewer: reviewer, Task: "FAC-659",
		BuilderFamily: reviewledger.FamilyUnrecorded, Gate: reviewledger.GateProvenanceUnrecorded,
	}); err != nil {
		t.Fatal(err)
	}
	family, err := provenBuilderFamily(root, sha)
	if err != nil {
		t.Fatal(err)
	}

	const callers = 8
	errs := make(chan error, callers)
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- completeReviewLaunchProvenance(root, "FAC-659", sha, "pool-01-7", family)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent completion: %v", err)
		}
	}

	rows, err := ledger.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	var records []reviewledger.LedgerRow
	for _, row := range rows {
		if row.Event == string(reviewledger.EventRecord) && row.SHA == sha && row.Reviewer == reviewer {
			records = append(records, row)
		}
	}
	if len(records) != 2 {
		t.Fatalf("record rows = %d, want immutable original plus one effective completion: %+v", len(records), records)
	}
	if records[0].BuilderFamily != reviewledger.FamilyUnrecorded || records[0].Gate != reviewledger.GateProvenanceUnrecorded {
		t.Fatalf("original unrecorded history was changed: %+v", records[0])
	}
	completed := records[1]
	if completed.BuilderFamily != "anthropic" || completed.Gate != "launch-provenance" ||
		completed.Task != "FAC-659" || completed.Lease != "pool-01-7" || completed.PatchURL == "" {
		t.Fatalf("receipt-backed completion lost a binding: %+v", completed)
	}

	if _, err := ledger.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: reviewer, Verdict: reviewledger.VerdictPASS,
		BuilderFamily: "anthropic", ReviewerFamily: "openai", Task: "FAC-659",
	}); err != nil {
		t.Fatal(err)
	}
	readiness, err := ledger.MergeReadinessFor(sha)
	if err != nil {
		t.Fatal(err)
	}
	if !readiness.Ready {
		t.Fatalf("different-family PASS did not become ready: %+v", readiness)
	}

	for _, tc := range []struct {
		name   string
		family string
	}{
		{name: "conflicting proven family", family: "openai"},
		{name: "unknown family", family: "mystery"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := completeReviewLaunchProvenance(root, "FAC-659", sha, "pool-01-7", tc.family); err == nil {
				t.Fatalf("family %q was accepted", tc.family)
			}
		})
	}
}

func fac659Candidate(t *testing.T) (root, sha string, commitAt time.Time) {
	t.Helper()
	t.Setenv("HERD_LAUNCH_RECEIPTS", "")
	t.Setenv("HERD_REVIEW_LEDGER", "")
	root = t.TempDir()
	fac659Git(t, root, "init", "-q", "-b", "main")
	fac659Git(t, root, "config", "user.email", "fac659@test.invalid")
	fac659Git(t, root, "config", "user.name", "fac659")
	fac659Git(t, root, "commit", "-q", "--allow-empty", "-m", "base")
	fac659Git(t, root, "branch", "origin/main")
	fac659Git(t, root, "checkout", "-q", "-b", "herd/fac-659")
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("FAC-659\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fac659Git(t, root, "add", "candidate.txt")
	fac659Git(t, root, "commit", "-q", "-m", "candidate")
	sha = fac659Git(t, root, "rev-parse", "HEAD")
	commitAt = committime.Of(root, sha)
	if commitAt.IsZero() {
		t.Fatal("candidate has no commit time")
	}
	return root, sha, commitAt
}

func fac659Git(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
