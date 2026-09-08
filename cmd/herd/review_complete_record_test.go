package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/internal/testgit"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestReviewCompleteRecordNativeRecovery(t *testing.T) {
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		b, e := testgit.Command(root, args...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "work")
	write := func(path string, b []byte) {
		t.Helper()
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(filepath.Join(root, "cmd/herd/fixture.go"), []byte("package main\n"))
	git("add", ".")
	git("commit", "-q", "-m", "candidate")
	sha := git("rev-parse", "HEAD")
	body := fmt.Sprintf("sha: %s\nbranch: work\ntask: FAC-759\nreviewer: independent-reviewer\nreviewer-family: google\nbuilder-family: openai\nverdict: PASS\nreviewed-base: %s\nreviewed-head: %s\n---\n## Tests run\ngo test ./... passed.\n%s", sha, base, sha, strings.Repeat("Independent review checked the failure path and exact candidate. ", 6))
	parsed := reviewingest.Parse(body)
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	artifact := filepath.Join(root, ".herd/review/inbox/retained.md")
	write(artifact, []byte(body))
	rows := []map[string]any{
		{"event": "record", "sha": sha, "reviewer": "independent-reviewer", "task": "FAC-759", "builder_family": "unrecorded", "gate": "provenance-unrecorded", "lease": "pool-real", "patch_url": "patch-real"},
		{"event": "verdict", "sha": sha, "candidate_sha": sha, "reviewer": "independent-reviewer", "task": "FAC-759", "builder_family": "openai", "reviewer_family": "google", "verdict": "PASS", "artifact": ".herd/review/inbox/retained.md", "artifact_digest": digest, "verification_digest": parsed.VerificationDigest()},
	}
	var original []byte
	for _, row := range rows {
		b, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		original = append(original, append(b, '\n')...)
	}
	ledgerPath := filepath.Join(root, ".herd/review-ledger.jsonl")
	write(ledgerPath, original)
	when, e := time.Parse(time.RFC3339, git("show", "-s", "--format=%cI", sha))
	if e != nil {
		t.Fatal(e)
	}
	receipt := fmt.Sprintf("{\"created_at\":%q,\"lane\":\"work\",\"branch\":\"work\",\"provider\":\"codex\",\"model\":\"test-model\",\"builder_family\":\"openai\",\"accepted\":true}\n", when.Add(-time.Minute).Format(time.RFC3339))
	receiptPath := filepath.Join(root, ".herd/launch-receipts.jsonl")
	write(receiptPath, []byte(receipt))
	run := func(extra ...string) ([]byte, error) {
		args := []string{"review-complete-record", "FAC-759", "--candidate", sha, "--reviewer", "independent-reviewer", "--artifact", artifact}
		args = append(args, extra...)
		c := exec.Command(binary, args...)
		c.Dir = root
		c.Env = append(os.Environ(), "HERD_PROJECT_ROOT="+root, "HERD_CANONICAL_ROOT="+root, "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath)
		return c.CombinedOutput()
	}
	// Distinct native processes share one ledger; only one record may append.
	type result struct {
		output []byte
		err    error
	}
	results := make(chan result, 4)
	for i := 0; i < 4; i++ {
		go func() { b, e := run(); results <- result{b, e} }()
	}
	for i := 0; i < 4; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("native record recovery: %v %s", r.err, r.output)
		}
	}
	l, e := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if e != nil {
		t.Fatal(e)
	}
	got, e := l.AllRows()
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 3 {
		t.Fatalf("concurrent recovery appended %d rows; want 3 total", len(got))
	}
	last := got[len(got)-1]
	if last.Event != "record" || last.BuilderFamily != "openai" || last.Tier != "R3" || last.Lease != "pool-real" || last.PatchURL != "patch-real" {
		t.Fatalf("incomplete recovered record: %+v", last)
	}
	admitted, e := l.AdmitReduced(reviewledger.ReducedAdmissionOpts{CandidateSHA: sha})
	if e != nil || !admitted.Admitted {
		t.Fatalf("receipt admission: %+v %v", admitted, e)
	}
	after, e := os.ReadFile(ledgerPath)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(string(after), string(original)) {
		t.Fatal("rewrote history")
	}
	if b, e := run(); e != nil {
		t.Fatalf("repeat: %v %s", e, b)
	}
	repeat, _ := os.ReadFile(ledgerPath)
	if string(repeat) != string(after) {
		t.Fatal("repeat appended")
	}
	write(receiptPath, nil)
	if _, e := run(); e == nil {
		t.Fatal("accepted missing native receipt")
	}
	write(receiptPath, []byte(receipt))
	write(artifact, []byte(body+"changed"))
	if _, e := run(); e == nil {
		t.Fatal("accepted changed artifact")
	}
	if _, e := run("--tier", "R0"); e == nil {
		t.Fatal("accepted operator tier assertion")
	}
	final, _ := os.ReadFile(ledgerPath)
	if string(final) != string(after) {
		t.Fatal("refusal mutated ledger")
	}
}

func TestReviewCompleteRecordRecoversLegacyBranchPlaceholder(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		b, e := testgit.Command(root, args...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("checkout", "-q", "-b", "recovery/fac-655-record-completion")
	write := func(path string, b []byte) {
		t.Helper()
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(filepath.Join(root, "cmd/herd/fixture.go"), []byte("package main\n"))
	git("add", ".")
	git("commit", "-q", "-m", "candidate")
	sha := git("rev-parse", "HEAD")
	branch := "recovery/fac-655-record-completion"
	body := fmt.Sprintf("sha: %s\nbranch: %s\ntask: FAC-655\nreviewer: independent-reviewer\nreviewer-family: anthropic\nbuilder-family: xai\nverdict: PASS\nreviewed-base: %s\nreviewed-head: %s\n---\n## Tests run\ngo test ./... passed.\n%s", sha, branch, base, sha, strings.Repeat("Independent review checked the failure path and exact candidate. ", 6))
	parsed := reviewingest.Parse(body)
	sum := sha256.Sum256([]byte(body))
	digest := hex.EncodeToString(sum[:])
	artifact := filepath.Join(root, ".herd/review/inbox/retained.md")
	write(artifact, []byte(body))
	rows := []map[string]any{
		{"event": "record", "sha": sha, "reviewer": "independent-reviewer", "task": branch, "builder_family": "unrecorded", "gate": "provenance-unrecorded", "lease": "pool-real", "patch_url": "patch-real"},
		{"event": "verdict", "sha": sha, "candidate_sha": sha, "reviewer": "independent-reviewer", "task": "FAC-655", "builder_family": "xai", "reviewer_family": "anthropic", "verdict": "PASS", "artifact": ".herd/review/inbox/retained.md", "artifact_digest": digest, "verification_digest": parsed.VerificationDigest()},
	}
	var original []byte
	for _, row := range rows {
		b, e := json.Marshal(row)
		if e != nil {
			t.Fatal(e)
		}
		original = append(original, append(b, '\n')...)
	}
	ledgerPath := filepath.Join(root, ".herd/review-ledger.jsonl")
	write(ledgerPath, original)
	write(filepath.Join(root, ".herd/harvest-queue.jsonl"), []byte(fmt.Sprintf("{\"event\":\"enqueue\",\"sha\":%q,\"reviewer\":\"independent-reviewer\",\"branch\":%q,\"status\":\"queued\"}\n", sha, branch)))
	when, e := time.Parse(time.RFC3339, git("show", "-s", "--format=%cI", sha))
	if e != nil {
		t.Fatal(e)
	}
	receipt := fmt.Sprintf("{\"created_at\":%q,\"lane\":%q,\"branch\":%q,\"provider\":\"grok\",\"model\":\"grok-4.6\",\"builder_family\":\"xai\",\"accepted\":true}\n", when.Add(-time.Minute).Format(time.RFC3339), branch, branch)
	write(filepath.Join(root, ".herd/launch-receipts.jsonl"), []byte(receipt))
	run := func() ([]byte, error) {
		c := exec.Command(binary, "review-complete-record", "FAC-655", "--candidate", sha, "--reviewer", "independent-reviewer", "--artifact", artifact)
		c.Dir = root
		c.Env = append(os.Environ(), "HERD_PROJECT_ROOT="+root, "HERD_CANONICAL_ROOT="+root, "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath)
		return c.CombinedOutput()
	}
	if out, err := run(); err != nil {
		t.Fatalf("native branch-placeholder recovery: %v %s", err, out)
	}
	l, e := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if e != nil {
		t.Fatal(e)
	}
	got, e := l.AllRows()
	if e != nil {
		t.Fatal(e)
	}
	if len(got) != 3 {
		t.Fatalf("recovery appended %d rows; want 3 total", len(got))
	}
	last := got[len(got)-1]
	if last.Event != "record" || last.Task != "FAC-655" || last.BuilderFamily != "xai" || last.ReviewerFamily != "anthropic" || last.Tier != "R3" || last.Gate != "independent" || last.Lease != "pool-real" || last.PatchURL != "patch-real" {
		t.Fatalf("incomplete recovered record: %+v", last)
	}
	queued, e := l.Queued()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, q := range queued {
		if q.SHA == sha {
			found = true
		}
	}
	if !found {
		t.Fatalf("Queued omitted recovered candidate: %+v", queued)
	}
	after, e := os.ReadFile(ledgerPath)
	if e != nil {
		t.Fatal(e)
	}
	if !strings.HasPrefix(string(after), string(original)) {
		t.Fatal("rewrote history")
	}
	if out, err := run(); err != nil {
		t.Fatalf("repeat: %v %s", err, out)
	}
	repeat, _ := os.ReadFile(ledgerPath)
	if string(repeat) != string(after) {
		t.Fatal("repeat appended")
	}
	verdicts := 0
	for _, row := range got {
		if row.Event == "verdict" {
			verdicts++
		}
	}
	if verdicts != 1 {
		t.Fatalf("manufactured verdict rows: %d", verdicts)
	}
	wrong := exec.Command(binary, "review-complete-record", "FAC-654", "--candidate", sha, "--reviewer", "independent-reviewer", "--artifact", artifact)
	wrong.Dir = root
	wrong.Env = append(os.Environ(), "HERD_PROJECT_ROOT="+root, "HERD_CANONICAL_ROOT="+root, "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath)
	if out, err := wrong.CombinedOutput(); err == nil {
		t.Fatalf("accepted wrong closeable task: %s", out)
	}
	final, _ := os.ReadFile(ledgerPath)
	if string(final) != string(after) {
		t.Fatal("wrong-task refusal mutated ledger")
	}
}

func TestReviewIngestCompletesPoolPlaceholderThenCompleteRecordIsIdempotent(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is unavailable: %v", err)
	}
	binary := buildHerd(t)
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		b, e := testgit.Command(root, args...).CombinedOutput()
		if e != nil {
			t.Fatalf("git %v: %v %s", args, e, b)
		}
		return strings.TrimSpace(string(b))
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	git("commit", "-q", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	git("branch", "origin/main")
	git("checkout", "-q", "-b", "recovery/fac-655-record-completion")
	write := func(path string, b []byte) {
		t.Helper()
		if e := os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			t.Fatal(e)
		}
		if e := os.WriteFile(path, b, 0600); e != nil {
			t.Fatal(e)
		}
	}
	write(filepath.Join(root, "cmd/herd/fixture.go"), []byte("package main\n"))
	git("add", ".")
	git("commit", "-q", "-m", "candidate")
	sha := git("rev-parse", "HEAD")
	branch := "recovery/fac-655-record-completion"
	placeholder, e := json.Marshal(map[string]any{"event": "record", "sha": sha, "reviewer": "independent-reviewer", "task": branch, "builder_family": "unrecorded", "gate": "provenance-unrecorded", "lease": "pool-real", "patch_url": "patch-real"})
	if e != nil {
		t.Fatal(e)
	}
	ledgerPath := filepath.Join(root, ".herd/review-ledger.jsonl")
	write(ledgerPath, append(placeholder, '\n'))
	when, e := time.Parse(time.RFC3339, git("show", "-s", "--format=%cI", sha))
	if e != nil {
		t.Fatal(e)
	}
	receipt := fmt.Sprintf("{\"created_at\":%q,\"lane\":%q,\"branch\":%q,\"provider\":\"grok\",\"model\":\"grok-4.6\",\"builder_family\":\"xai\",\"accepted\":true}\n", when.Add(-time.Minute).Format(time.RFC3339), branch, branch)
	write(filepath.Join(root, ".herd/launch-receipts.jsonl"), []byte(receipt))
	body := fmt.Sprintf("sha: %s\nbranch: %s\ntask: FAC-655\nreviewer: independent-reviewer\nreviewer-family: anthropic\nbuilder-family: xai\nverdict: PASS\nreviewed-base: %s\nreviewed-head: %s\n---\n## Tests run\ngo test ./... passed.\n%s", sha, branch, base, sha, strings.Repeat("Independent review checked the failure path and exact candidate. ", 6))
	artifact := filepath.Join(root, "verdict.md")
	write(artifact, []byte(body))
	cmd := exec.Command(binary, "review-ingest", "verdict.md")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "HERD_PROJECT_ROOT="+root, "HERD_CANONICAL_ROOT="+root, "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath, "HERD_LAUNCH_RECEIPTS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("review-ingest: %v %s", err, out)
	}
	l, e := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if e != nil {
		t.Fatal(e)
	}
	got, e := l.AllRows()
	if e != nil {
		t.Fatal(e)
	}
	var lastRecord *reviewledger.LedgerRow
	verdicts := 0
	for i := range got {
		if got[i].Event == "record" {
			lastRecord = &got[i]
		}
		if got[i].Event == "verdict" {
			verdicts++
		}
	}
	if lastRecord == nil || lastRecord.Task != "FAC-655" || lastRecord.BuilderFamily != "xai" || lastRecord.ReviewerFamily != "anthropic" || lastRecord.Tier != "R3" || lastRecord.Gate != "independent" || lastRecord.Lease != "pool-real" || lastRecord.PatchURL != "patch-real" {
		t.Fatalf("ingest did not complete placeholder: %+v rows=%+v", lastRecord, got)
	}
	if verdicts != 1 {
		t.Fatalf("verdict count = %d", verdicts)
	}
	queued, e := l.Queued()
	if e != nil {
		t.Fatal(e)
	}
	found := false
	for _, q := range queued {
		if q.SHA == sha {
			found = true
		}
	}
	if !found {
		t.Fatalf("Queued omitted ingested candidate: %+v", queued)
	}
	retained := artifact
	for _, row := range got {
		if row.Event == "verdict" && strings.TrimSpace(row.Artifact) != "" {
			retained = filepath.Join(root, row.Artifact)
		}
	}
	c := exec.Command(binary, "review-complete-record", "FAC-655", "--candidate", sha, "--reviewer", "independent-reviewer", "--artifact", retained)
	c.Dir = root
	c.Env = append(os.Environ(), "HERD_PROJECT_ROOT="+root, "HERD_CANONICAL_ROOT="+root, "HERD_ROOT="+root, "HERD_REVIEW_LEDGER="+ledgerPath)
	afterIngest, _ := os.ReadFile(ledgerPath)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("complete-record after ingest: %v %s", err, out)
	}
	repeat, _ := os.ReadFile(ledgerPath)
	if string(repeat) != string(afterIngest) {
		t.Fatal("complete-record after ingest appended extra rows")
	}
}
