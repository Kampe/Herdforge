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
