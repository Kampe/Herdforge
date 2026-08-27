package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-620, fourth review. Every rejection path in the provenance join lands on
// "no qualifying receipt", and that outcome LEFT THE ARTIFACT ALONE -- so each
// time the join got stricter, MORE reviewer-asserted families went unchecked.
// The guard widened the hole it existed to close.
//
// This runs the real binary. The first two attempts at this card shipped a
// sound helper with no production caller and a production caller with no
// coverage; a test that drives RequireCorroboratedFamily directly would repeat
// that mistake for a third time. Deleting the call from runReviewIngest must
// turn these red.

// corroborationRepo builds a real candidate with a real diff, so that earlier
// gates (unresolvable sha, empty diff, bad task ref) cannot mask the gate
// under test.
func corroborationRepo(t *testing.T) (repo, sha string) {
	t.Helper()
	repo = t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", repo}, args...)...)
		c.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "base")
	git("update-ref", "refs/remotes/origin/main", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "candidate.txt"), []byte("change\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "candidate.txt")
	git("commit", "-q", "-m", "candidate")
	out, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	return repo, strings.TrimSpace(string(out))
}

func writeVerdict(t *testing.T, repo, sha, family string) {
	t.Helper()
	inbox := filepath.Join(repo, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "sha: " + sha + "\n" +
		"branch: wt/candidate\n" +
		"task: FAC-620\n" +
		"reviewer: review-corroboration\n" +
		"reviewer-family: anthropic\n" +
		"builder-family: " + family + "\n" +
		"verdict: PASS\n" +
		"reviewed-head: " + sha + "\n---\n" +
		strings.Repeat("Body long enough to clear the minimum-length gate. ", 12) + "\n"
	if err := os.WriteFile(filepath.Join(inbox, "sha-review-corroboration.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runIngest(t *testing.T, binary, repo string) string {
	t.Helper()
	cmd := exec.Command(binary, "review-ingest", "--sweep", "--dry-run")
	cmd.Dir = repo
	// No receipt log and no ledger rows: nothing can corroborate a claim.
	cmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// recordedFamily ingests for real and returns what the LEDGER ended up holding.
// The dry-run summary does not print the family, and asserting on a line that
// cannot show the field would be a test that passes regardless -- the vacuity
// this card has already been failed for twice.
func recordedFamily(t *testing.T, binary, repo string) string {
	t.Helper()
	cmd := exec.Command(binary, "review-ingest", "--sweep")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("ingest failed: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(repo, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("no ledger written: %v", err)
	}
	return string(raw)
}

// THE regression. A bare family assertion with no launch receipt reaching the
// commit and no ledger record must not be TRUSTED by the shipped command: it is
// routed to provenance-unrecorded, where it cannot decide independence.
//
// Downgraded, not refused. Refusing was tried and measured against the live
// inbox: it refused the only artifact there, a legitimate independent FAIL,
// because the ledger record that corroborates a SHA is created BY ingesting a
// verdict for that SHA. No first review of any commit can ever be corroborated.
func TestIngestDoesNotTrustAnUncorroboratedBuilderFamily(t *testing.T) {
	binary := buildHerd(t)
	repo, sha := corroborationRepo(t)
	writeVerdict(t, repo, sha, "openai")

	if text := runIngest(t, binary, repo); strings.Contains(text, "refused=1") {
		t.Fatalf("the shipped ingest REFUSED an unproven family instead of downgrading it; "+
			"that halts every first review of every commit.\nGot:\n%s", text)
	}

	ledger := recordedFamily(t, binary, repo)
	if strings.Contains(ledger, `"builder_family":"openai"`) {
		t.Fatalf("the ledger RECORDED a builder-family that nothing proves. Independence is "+
			"computed against this field, so an unproven family is a confident wrong answer -- "+
			"the harm FAC-620 exists to prevent.\nLedger:\n%s", ledger)
	}
	if !strings.Contains(ledger, "unrecorded") {
		t.Fatalf("the unproven family was not routed to unrecorded provenance.\nLedger:\n%s", ledger)
	}
}

// The escape hatch must stay open through the same shipped path. A reviewer who
// candidly writes "unknown" is being honest, and ingest routes that to
// provenance-unrecorded. Refusing it would punish honesty and reward assertion
// -- the FAC-608 defect this repository already paid for once.
func TestIngestStillAdmitsACandidlyUnrecordedFamily(t *testing.T) {
	binary := buildHerd(t)
	repo, sha := corroborationRepo(t)
	writeVerdict(t, repo, sha, "unknown")

	text := runIngest(t, binary, repo)
	if strings.Contains(text, "refused=1") {
		t.Fatalf("a candid \"unknown\" was refused; the corroboration gate is firing on the "+
			"honest case it is supposed to exempt, which punishes candour and rewards "+
			"assertion -- the FAC-608 defect.\nGot:\n%s", text)
	}
}

// Ordering. The corroboration gate is broad, so running it before Validate made
// it MASK every more specific diagnostic: a malformed task ref came back as
// "unproven family" and an operator could not see which ref was actually wrong.
func TestAMoreSpecificRefusalIsNotMaskedByTheCorroborationGate(t *testing.T) {
	binary := buildHerd(t)
	repo, sha := corroborationRepo(t)

	inbox := filepath.Join(repo, ".herd", "review", "inbox")
	if err := os.MkdirAll(inbox, 0o755); err != nil {
		t.Fatal(err)
	}
	const offending = "standing/docs-custodian"
	body := "sha: " + sha + "\nbranch: " + offending + "\ntask: " + offending + "\n" +
		"reviewer: review-masking\nreviewer-family: anthropic\nbuilder-family: openai\n" +
		"verdict: PASS\nreviewed-head: " + sha + "\n---\n" +
		strings.Repeat("Body long enough to clear the minimum-length gate. ", 12) + "\n"
	if err := os.WriteFile(filepath.Join(inbox, "sha-review-masking.md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	text := runIngest(t, binary, repo)
	if !strings.Contains(text, offending) {
		t.Fatalf("the corroboration gate masked the task-ref diagnostic; an operator cannot see "+
			"WHICH ref was wrong.\nGot:\n%s", text)
	}
}
