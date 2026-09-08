package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/harness"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/router"
)

func TestParseReviewHostIngestRefusesFlagOnlyTrust(t *testing.T) {
	_, err := parseReviewHostIngestArgs([]string{
		"--candidate", strings.Repeat("a", 40),
		"--reviewer", "review-fac-652-2a3a20d57ba7",
		"--receipt", "receipt.json",
		"--host", "w4",
	})
	if err == nil || !strings.Contains(err.Error(), "not authentication") {
		t.Fatalf("--host error = %v", err)
	}
	_, err = parseReviewHostIngestArgs([]string{
		"--candidate", strings.Repeat("a", 40),
		"--reviewer", "review-fac-652-2a3a20d57ba7",
		"--receipt", "receipt.json",
		"--family", "openai",
	})
	if err == nil || !strings.Contains(err.Error(), "not authentication") {
		t.Fatalf("--family error = %v", err)
	}
}

// TestForgedReceiptRejectedByHostIngest is the executed FAC-765 Finding 1
// regression: a hand-written launch.Receipt JSON that was never emitted by
// pkg/launch and never appended to .herd/launch-receipts.jsonl must not
// authenticate host-ingest. The independent review proved the current CLI
// admits PaneID=W4-forged-never-ran as an independent record; this test
// watches that gap RED before the canonical-membership repair and GREEN after.
func TestForgedReceiptRejectedByHostIngest(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, fx.reviewReceipt)
	forged := fx.builderReceipt
	forged.PaneID = "W4-forged-never-ran"
	forged.HerdrSession = "W4-forged-never-ran"
	forged.ProcessIdentity = "W4-forged-never-ran"
	forged.BuilderFamily = "xai"
	forged.DecisionDigest = "forged-digest-never-ran"
	forged.StartToken = "forged-start"
	locator := filepath.Join(fx.dir, "forged-receipt.json")
	writeJSON(t, locator, forged)

	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil || !strings.Contains(err.Error(), "canonical accepted member") {
		t.Fatalf("forged receipt must be refused as nonmember, err=%v", err)
	}
	assertLedgerHostAbsent(t, fx, "W4-forged-never-ran")
}

func TestCanonicalReviewLaunchAuthenticatesHostIngest(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, fx.reviewReceipt)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, fx.reviewReceipt)

	if err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	}); err != nil {
		t.Fatalf("exact accepted review locator must authenticate: %v", err)
	}
	assertLedgerHostPresent(t, fx, "W4-canonical-review-pane")
	assertLedgerHostAbsent(t, fx, "builder-pane-not-reviewer")
	assertLedgerHostAbsent(t, fx, "builder-proc")
	assertLedgerHostAbsent(t, fx, "W4-forged-never-ran")
}

func TestHostIngestRefusesMismatchedCanonicalHost(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, fx.reviewReceipt)
	spoof := fx.reviewReceipt
	spoof.PaneID = "W4-mismatched-host"
	locator := filepath.Join(fx.dir, "mismatched-host.json")
	writeJSON(t, locator, spoof)

	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil || !strings.Contains(err.Error(), "mismatched host") {
		t.Fatalf("locator pane must not override canonical review host, err=%v", err)
	}
	assertLedgerHostAbsent(t, fx, "W4-mismatched-host")
}

func TestHostIngestRefusesMismatchedCanonicalSession(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, fx.reviewReceipt)
	spoof := fx.reviewReceipt
	spoof.HerdrSession = "mismatched-review-session"
	locator := filepath.Join(fx.dir, "mismatched-session.json")
	writeJSON(t, locator, spoof)

	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil || !strings.Contains(err.Error(), "mismatched session") {
		t.Fatalf("locator session must not override canonical review session, err=%v", err)
	}
	assertLedgerHostAbsent(t, fx, "mismatched-review-session")
}

func TestHostIngestRefusesBuilderPaneAsReviewerHost(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt)
	locator := filepath.Join(fx.dir, "builder-only.json")
	writeJSON(t, locator, fx.builderReceipt)

	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil || !strings.Contains(err.Error(), "review launch") {
		t.Fatalf("builder pane/session/cwd is not reviewer host proof, err=%v", err)
	}
	assertLedgerHostAbsent(t, fx, "builder-pane-not-reviewer")
	assertLedgerHostAbsent(t, fx, "builder-proc")
}

func TestCanonicalReviewProvenanceRejectsDifferentCandidate(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, fx.reviewReceipt)
	if err := os.WriteFile(filepath.Join(fx.dir, "later"), []byte("other-candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHostIngest(t, fx.dir, []string{
		"GIT_AUTHOR_DATE=2026-09-07T13:00:00+00:00",
		"GIT_COMMITTER_DATE=2026-09-07T13:00:00+00:00",
	}, "add", "-A")
	gitHostIngest(t, fx.dir, []string{
		"GIT_AUTHOR_DATE=2026-09-07T13:00:00+00:00",
		"GIT_COMMITTER_DATE=2026-09-07T13:00:00+00:00",
	}, "commit", "-qm", "later candidate")
	other := gitHostIngest(t, fx.dir, nil, "rev-parse", "HEAD")
	if other == fx.sha {
		t.Fatal("fixture did not produce a second candidate SHA")
	}
	proof, err := resolveCanonicalLaunchProvenance(fx.dir, fx.builderReceipt, fx.reviewer, other, "",
		commitTimeOf(fx.dir, other), func(branch, sha string) bool { return branchReaches(fx.dir, branch, sha) })
	if err == nil {
		t.Fatalf("review launch for %s authenticated host=%s for other candidate %s", fx.sha, proof.Host, other)
	}
	if !strings.Contains(err.Error(), "review launch") {
		t.Fatalf("cross-candidate refusal = %v, want review launch binding", err)
	}
}

func TestCanonicalReviewProvenanceRejectsDifferentTask(t *testing.T) {
	fx := newHostIngestFixture(t)
	foreign := fx.reviewReceipt
	foreign.TaskRef = "FAC-999"
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, foreign)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, foreign)
	artifact := filepath.Join(fx.dir, "review.md")
	if err := os.WriteFile(artifact, []byte(""+
		"sha: "+fx.sha+"\n"+
		"reviewer: "+fx.reviewer+"\n"+
		"task: FAC-765\n"+
		"verdict: PASS\n"+
		"reviewer-family: google\n"+
		"builder-family: openai\n"+
		"reviewed-base: "+fx.sha+"\n"+
		"reviewed-head: "+fx.sha+"\n"+
		"---\nbody\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
		"--artifact", artifact,
		"--base", fx.sha,
	})
	if err == nil || !(strings.Contains(err.Error(), "task") || strings.Contains(err.Error(), "review launch")) {
		t.Fatalf("review launch for FAC-999 must not authenticate FAC-765, err=%v", err)
	}
	assertLedgerHostAbsent(t, fx, "W4-canonical-review-pane")
}

func TestCanonicalReviewProvenanceRefusesOmittedTaskRepoLane(t *testing.T) {
	fx := newHostIngestFixture(t)
	blank := fx.reviewReceipt
	blank.TaskRef = ""
	blank.Repository = ""
	blank.Lane = ""
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, blank)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, blank)
	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil {
		t.Fatal("omitted task/repository/lane must not authenticate HostIngest")
	}
	assertLedgerHostAbsent(t, fx, "W4-canonical-review-pane")
}

func TestCanonicalReviewProvenanceRefusesContradictoryRepoAndLane(t *testing.T) {
	fx := newHostIngestFixture(t)
	wrong := fx.reviewReceipt
	wrong.Repository = "other.example/not-this-repo"
	wrong.Lane = "other-lane"
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, wrong)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, wrong)
	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil {
		t.Fatal("contradictory repository/lane must not authenticate HostIngest")
	}
	assertLedgerHostAbsent(t, fx, "W4-canonical-review-pane")
}

func TestNativeStartedReviewReceiptAuthenticatesHostIngest(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt)
	d := &router.LaunchDecision{
		Role: router.RoleReviewer, Shape: "qa", Provider: "claude", Model: "claude-opus",
		CandidateSHA: fx.sha, Argv: []string{"claude"}, Family: "anthropic",
	}
	req := launch.Request{
		Decision: d, HookDiscovery: harness.NoHooksDiscovery(),
		TaskRef: "FAC-765", Name: fx.reviewer, Lane: fx.reviewer,
		Repository: "example.test/herdforge",
		PaneID:     "W4-canonical-review-pane", HerdrSession: "w4-review-session",
		ProcessIdentity: "W4-canonical-review-pane", StartToken: "native-review-start",
	}
	if err := launch.RecordStarted(req, &launch.JSONLSink{Path: fx.receipts}); err != nil {
		t.Fatalf("RecordStarted: %v", err)
	}
	members, err := launch.ReadReceipts(fx.receipts)
	if err != nil {
		t.Fatal(err)
	}
	var native launch.Receipt
	for _, m := range members {
		if m.Role == launch.ReviewerRole && m.Accepted {
			native = m
		}
	}
	if native.CandidateSHA != fx.sha {
		t.Fatalf("native receipt CandidateSHA=%q want %s", native.CandidateSHA, fx.sha)
	}
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, native)
	if err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	}); err != nil {
		t.Fatalf("native reviewer receipt must authenticate: %v", err)
	}
	assertLedgerHostPresent(t, fx, "W4-canonical-review-pane")
}

func TestCanonicalReviewProvenanceRejectsBuilderLocator(t *testing.T) {
	fx := newHostIngestFixture(t)
	writeCanonicalLog(t, fx.receipts, fx.builderReceipt, fx.reviewReceipt)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, fx.builderReceipt)
	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil || !strings.Contains(err.Error(), "review launch") {
		t.Fatalf("builder locator must not authenticate this review, err=%v", err)
	}
	assertLedgerHostAbsent(t, fx, "W4-canonical-review-pane")
}

func TestCanonicalReviewProvenanceAcceptsPreEditBuilderEmptyCandidateSHA(t *testing.T) {
	fx := newHostIngestFixture(t)
	preEdit := fx.builderReceipt
	preEdit.CandidateSHA = ""
	writeCanonicalLog(t, fx.receipts, preEdit, fx.reviewReceipt)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, fx.reviewReceipt)

	if err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	}); err != nil {
		t.Fatalf("pre-edit builder receipt with empty CandidateSHA must still reach: %v", err)
	}
	assertLedgerHostPresent(t, fx, "W4-canonical-review-pane")
}

func TestHostIngestRefusesMissingCanonicalLog(t *testing.T) {
	fx := newHostIngestFixture(t)
	locator := filepath.Join(fx.dir, "locator.json")
	writeJSON(t, locator, fx.builderReceipt)

	err := runReviewHostIngest([]string{
		"--candidate", fx.sha,
		"--reviewer", fx.reviewer,
		"--receipt", locator,
	})
	if err == nil || !strings.Contains(err.Error(), "canonical launch log is missing") {
		t.Fatalf("missing log must refuse, err=%v", err)
	}
}

type hostIngestFixture struct {
	dir, sha, branch, ledger, receipts, reviewer string
	builderReceipt, reviewReceipt                launch.Receipt
}

func newHostIngestFixture(t *testing.T) hostIngestFixture {
	t.Helper()
	dir := t.TempDir()
	branch := "fix/fac-999"
	ledger := filepath.Join(dir, ".herd", "review-ledger.jsonl")
	receipts := filepath.Join(dir, ".herd", "launch-receipts.jsonl")
	t.Setenv("HERD_PROJECT_ROOT", dir)
	t.Setenv("HERD_REVIEW_LEDGER", ledger)
	t.Setenv("HERD_LAUNCH_RECEIPTS", receipts)
	t.Chdir(dir)

	gitHostIngest(t, dir, nil, "init", "-q", "-b", branch)
	gitHostIngest(t, dir, nil, "remote", "add", "origin", "https://example.test/herdforge.git")
	if err := os.WriteFile(filepath.Join(dir, "work"), []byte("fac-999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitHostIngest(t, dir, []string{
		"GIT_AUTHOR_DATE=2026-09-07T12:00:00+00:00",
		"GIT_COMMITTER_DATE=2026-09-07T12:00:00+00:00",
	}, "add", "-A")
	gitHostIngest(t, dir, []string{
		"GIT_AUTHOR_DATE=2026-09-07T12:00:00+00:00",
		"GIT_COMMITTER_DATE=2026-09-07T12:00:00+00:00",
	}, "commit", "-qm", "fac-999 work")
	sha := gitHostIngest(t, dir, nil, "rev-parse", "HEAD")

	reviewer := "review-fac-999"
	l, err := reviewledger.NewReviewLedger(dir, ledger)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: branch, Reviewer: reviewer, Task: "FAC-765",
		BuilderFamily: "openai", ReviewerFamily: "google", Gate: "independent",
		Artifact: "local-google.md",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Reviewer: reviewer, Task: "FAC-765", Branch: branch,
		Verdict: reviewledger.VerdictPASS, ReviewerFamily: "google", BuilderFamily: "openai",
		ArtifactDigest: "465571760dd09435214e1798905839079d1e02dc6ea741d49da0d95fbc77bc7c",
		Artifact:       "local-google.md", VfyDigest: "local-vfy", CandidateSHA: sha,
	}); err != nil {
		t.Fatal(err)
	}

	builder := launch.Receipt{
		CreatedAt:       time.Date(2026, 9, 7, 3, 9, 34, 0, time.UTC),
		TaskRef:         "FAC-765",
		Role:            launch.WorkerRole,
		TaskShape:       launch.Implementation,
		Provider:        "codex",
		Model:           "gpt-5.6-luna",
		Effort:          "medium",
		DecisionDigest:  "builder-digest-001",
		Argv:            []string{"codex", "--model", "gpt-5.6-luna"},
		Accepted:        true,
		Name:            "fac-765-builder",
		PaneID:          "builder-pane-not-reviewer",
		Repository:      "example.test/herdforge",
		Lane:            "builder",
		BuilderFamily:   "openai",
		Branch:          branch,
		CandidateSHA:    sha,
		HerdrSession:    "builder-session",
		CWD:             dir,
		ProcessIdentity: "builder-proc",
		StartToken:      "builder-start",
		PacketDigest:    "builder-packet",
	}
	review := launch.Receipt{
		CreatedAt:       time.Date(2026, 9, 7, 14, 0, 0, 0, time.UTC),
		TaskRef:         "FAC-765",
		Role:            launch.ReviewerRole,
		TaskShape:       "qa",
		Provider:        "claude",
		Model:           "claude-opus",
		Effort:          "high",
		DecisionDigest:  "review-digest-001",
		Argv:            []string{"claude", "--model", "claude-opus"},
		Accepted:        true,
		Name:            reviewer,
		PaneID:          "W4-canonical-review-pane",
		Repository:      "example.test/herdforge",
		Lane:            reviewer,
		Branch:          branch,
		CandidateSHA:    sha,
		HerdrSession:    "w4-review-session",
		ProcessIdentity: "W4-canonical-review-pane",
		StartToken:      "review-start",
		PacketDigest:    "review-packet",
	}
	return hostIngestFixture{
		dir: dir, sha: sha, branch: branch, ledger: ledger, receipts: receipts,
		reviewer: reviewer, builderReceipt: builder, reviewReceipt: review,
	}
}

func gitHostIngest(t *testing.T, dir string, extra []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null"), extra...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func writeJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCanonicalLog(t *testing.T, path string, receipts ...launch.Receipt) {
	t.Helper()
	sink := &launch.JSONLSink{Path: path}
	for _, r := range receipts {
		if err := sink.Write(r); err != nil {
			t.Fatal(err)
		}
	}
}

func assertLedgerHostPresent(t *testing.T, fx hostIngestFixture, host string) {
	t.Helper()
	rows := ledgerRows(t, fx)
	for _, r := range rows {
		if strings.TrimSpace(r.Host) == host {
			return
		}
	}
	t.Fatalf("ledger missing authenticated host %q: %+v", host, rows)
}

func assertLedgerHostAbsent(t *testing.T, fx hostIngestFixture, host string) {
	t.Helper()
	rows := ledgerRows(t, fx)
	for _, r := range rows {
		if strings.TrimSpace(r.Host) == host {
			t.Fatalf("ledger recorded unauthenticated host %q: %+v", host, r)
		}
	}
}

func ledgerRows(t *testing.T, fx hostIngestFixture) []reviewledger.LedgerRow {
	t.Helper()
	l, err := reviewledger.NewReviewLedger(fx.dir, fx.ledger)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
