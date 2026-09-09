package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/mail"
)

// FAC-740. The broker deliver-then-readback loop could leave a verdict with
// no canonical artifact (FAC-737 seq587, FAC-738 seq584 resolved with no
// inbox entry), and nothing could ever materialize one: the control drain
// cannot consume broker callbacks and the FAC-373 retain path only copies an
// artifact that already exists. This runs the REAL binary against a delivered
// 737/738-shaped effect and its intent-only 739-shaped sibling: recovery must
// materialize the delivered effects for admission and refuse the intent-only
// record, because an intent was never delivered and never signed as an effect.
// Deleting the --recover-verdict wiring from reviewingest.go must turn these
// red.

// signDeliveredEffect reconstructs the broker-composed canonical body for a
// delivered verdict effect, exactly as serveBrokerConn composes it.
func signDeliveredEffect(t *testing.T, signer *dispatch.Signer, task, candidate, base, leaseID string, gen int64, verdict, detail string) string {
	t.Helper()
	line := "REVIEW VERDICT " + task + ": " + verdict + " candidate=" + candidate +
		" base=" + base + " lease=" + leaseID + " lease_gen=" + itoa64(gen) +
		" reviewer-bound (FAC-145)"
	if detail != "" {
		line += " — " + detail
	}
	effect := task + ":" + candidate + ":gen" + itoa64(gen) + ":" + leaseID + ":" + verdict
	effectID := mail.VerdictEffectID("herdforge:" + effect)
	sig, err := signer.SignBytes([]byte("herd-verdict-effect:" + effectID + "\n" + line))
	if err != nil {
		t.Fatal(err)
	}
	return line + " [effect " + effectID + " sig=" + sig + "]"
}

func itoa64(n int64) string { return strconv.FormatInt(n, 10) }

// postDeliveredVerdict replays the durable bus state of a delivered verdict.
func postDeliveredVerdict(t *testing.T, repo, task, candidate, body, dedupeID string) int64 {
	t.Helper()
	mb := mail.NewMailbox(mail.CallbackMailPath(repo))
	env, err := mb.PostCallback("coordinator", mail.Callback{
		Ref: task, Kind: mail.CallbackComplete, SHA: candidate,
		Detail: body, Repo: "herdforge", LeaseGeneration: 1,
		SenderRole: "coordinator", DedupeID: dedupeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	return env.Sequence
}

func runRecover(t *testing.T, binary, repo, record string) (string, error) {
	t.Helper()
	cmd := exec.Command(binary, "review-ingest", "--recover-verdict", record)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// signRecoveryRecord drives the REAL --sign-recovery-record surface: the
// coordinator key signs the record's reviewer/verification binding to the
// effect, and the signed record comes back on stdout (FAC-351). Recovery
// refuses unsigned records, so this is the only way a record becomes
// recoverable end to end. keyDir is the fixture's key directory — the SAME
// coordinator key the effect signatures and the published receipt.pub use.
func signRecoveryRecord(t *testing.T, binary, repo, keyDir string, rec map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := filepath.Join(t.TempDir(), "unsigned-record.json")
	if err := os.WriteFile(unsigned, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "review-ingest", "--sign-recovery-record", unsigned)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo, "HERD_KEY_DIR="+keyDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("sign recovery record: %v\n%s", err, out)
	}
	signed := filepath.Join(t.TempDir(), "signed-record.json")
	if err := os.WriteFile(signed, out, 0o600); err != nil {
		t.Fatal(err)
	}
	return signed
}

func TestRecoverVerdictMaterializesDeliveredEffectsForAdmission(t *testing.T) {
	binary := buildHerd(t)
	repo, candidate := corroborationRepo(t)
	baseOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD~1").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(string(baseOut))

	keyDir := t.TempDir()
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	// The signing surface resolves the key by the SAME repository identity
	// the CLI derives from the config, so the fixture key must be stored
	// under that identity for the CLI to load (never rotate) it.
	identity, idErr := dispatch.RepositoryIdentity(repo, "herdforge")
	if idErr != nil {
		t.Fatal(idErr)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, identity, repo)
	if err != nil {
		t.Fatal(err)
	}
	// The signing surface resolves the coordinator key through the project
	// config, exactly as every other signing command does.
	if err := os.MkdirAll(filepath.Join(repo, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".herd", "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: herdforge\ntask_provider:\n  type: memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// FAC-737 shape: an APPROVED verdict delivered and read back, durable
	// intent and delivered record on the bus, and NO inbox artifact.
	body := signDeliveredEffect(t, signer, "FAC-737", candidate, base, "claim:318", 1, "APPROVED", "")
	effect := "FAC-737:" + candidate + ":gen1:claim:318:APPROVED"
	seq := postDeliveredVerdict(t, repo, "FAC-737", candidate, body, mail.VerdictEffectID("herdforge:"+effect))
	if entries, _ := os.ReadDir(filepath.Join(repo, ".herd", "review", "inbox")); len(entries) != 0 {
		t.Fatalf("precondition: no canonical artifact may exist before recovery, got %d", len(entries))
	}

	rec := map[string]any{
		"repo": "herdforge", "ref": "FAC-737", "candidate_sha": candidate,
		"base_sha": base, "branch": "main", "lease_id": "claim:318",
		"lease_generation": 1, "verdict": "APPROVED", "reviewer": "reviewer-fac737",
		"reviewer_family": "anthropic", "bus_sequence": seq, "canonical_body": body,
	}
	recPath := signRecoveryRecord(t, binary, repo, keyDir, rec)

	out, recoverErr := runRecover(t, binary, repo, recPath)
	if recoverErr != nil {
		t.Fatalf("recovery of a delivered effect must succeed: %v\n%s", recoverErr, out)
	}
	if !strings.Contains(out, "RECOVERED") {
		t.Fatalf("recovery must report the materialized artifact:\n%s", out)
	}
	inbox := filepath.Join(repo, ".herd", "review", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil || len(entries) != 1 {
		t.Fatalf("recovery must materialize exactly one artifact: %v %v", entries, err)
	}
	materialized, _ := os.ReadFile(filepath.Join(inbox, entries[0].Name()))
	if !strings.Contains(string(materialized), "verdict: PASS") || !strings.Contains(string(materialized), "reviewer-family: anthropic") {
		t.Fatalf("recovered artifact must carry the admission-facing header:\n%s", materialized)
	}

	// The recovered artifact must ADMIT through the ordinary sweep: the seam
	// closes only when the evidence becomes ledger truth.
	cmd := exec.Command(binary, "review-ingest", "--sweep")
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recovered artifact must admit: %v\n%s", err, out)
	}
	rawLedger, err := os.ReadFile(filepath.Join(repo, ".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("no ledger written: %v", err)
	}
	// The ledger is JSONL; the verdict row is the admission outcome.
	var admitted struct {
		SHA     string `json:"sha"`
		Verdict string `json:"verdict"`
	}
	found := false
	for _, line := range strings.Split(strings.TrimSpace(string(rawLedger)), "\n") {
		var row struct {
			Event   string `json:"event"`
			SHA     string `json:"sha"`
			Verdict string `json:"verdict"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			t.Fatalf("ledger row must parse: %v\n%s", err, line)
		}
		if row.Event == "verdict" {
			admitted.SHA, admitted.Verdict, found = row.SHA, row.Verdict, true
		}
	}
	if !found {
		t.Fatalf("no verdict row admitted:\n%s", rawLedger)
	}
	if admitted.SHA != candidate || admitted.Verdict != "PASS" {
		t.Fatalf("ledger must hold the recovered verdict, got %+v", admitted)
	}
}

func TestRecoverVerdictRefusesIntentOnlyAndSupersededEvidence(t *testing.T) {
	binary := buildHerd(t)
	repo, candidate := corroborationRepo(t)
	baseOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD~1").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(string(baseOut))

	keyDir := t.TempDir()
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	// The signing surface resolves the key by the SAME repository identity
	// the CLI derives from the config, so the fixture key must be stored
	// under that identity for the CLI to load (never rotate) it.
	identity, idErr := dispatch.RepositoryIdentity(repo, "herdforge")
	if idErr != nil {
		t.Fatal(idErr)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, identity, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".herd", "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: herdforge\ntask_provider:\n  type: memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// FAC-738 then FAC-739: gen2 delivered and read back, then the gen3
	// attempt recorded its intent and stalled — the intent is on the bus but
	// was never delivered and never signed as an effect.
	deliveredBody := signDeliveredEffect(t, signer, "FAC-738", candidate, base, "claim:584", 2, "REJECTED", "")
	postDeliveredVerdict(t, repo, "FAC-738", candidate, deliveredBody, mail.VerdictEffectID("herdforge:FAC-738:"+candidate+":gen2:claim:584:REJECTED"))
	intentLine := "REVIEW VERDICT FAC-739: APPROVED candidate=" + candidate + " base=" + base +
		" lease=claim:592 lease_gen=1 reviewer-bound (FAC-145) — intent (undelivered)"

	// (a) The intent-only record is refused: not delivered, not signed.
	intentRec, _ := json.Marshal(map[string]any{
		"repo": "herdforge", "ref": "FAC-739", "candidate_sha": candidate,
		"base_sha": base, "branch": "main", "lease_id": "claim:592",
		"lease_generation": 1, "verdict": "APPROVED", "reviewer": "reviewer-fac739",
		"reviewer_family": "anthropic", "bus_sequence": 0, "canonical_body": intentLine,
	})
	intentPath := filepath.Join(t.TempDir(), "fac739-intent.json")
	if err := os.WriteFile(intentPath, intentRec, 0o600); err != nil {
		t.Fatal(err)
	}
	out, intentErr := runRecover(t, binary, repo, intentPath)
	if intentErr == nil {
		t.Fatalf("an intent-only record must be refused:\n%s", out)
	}
	if !strings.Contains(out, "REFUSED") {
		t.Fatalf("refusal must be explicit:\n%s", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(repo, ".herd", "review", "inbox")); len(entries) != 0 {
		t.Fatalf("a refused record must materialize nothing, got %d artifacts", len(entries))
	}

	// (b) A record superseded by the later delivered effect is refused...
	stalePath := signRecoveryRecord(t, binary, repo, keyDir, map[string]any{
		"repo": "herdforge", "ref": "FAC-738", "candidate_sha": candidate,
		"base_sha": base, "branch": "main", "lease_id": "claim:584",
		"lease_generation": 1, "verdict": "APPROVED", "reviewer": "reviewer-fac738",
		"reviewer_family": "anthropic", "bus_sequence": 0, "canonical_body": signDeliveredEffect(t, signer, "FAC-738", candidate, base, "claim:584", 1, "APPROVED", ""),
	})
	out, staleErr := runRecover(t, binary, repo, stalePath)
	if staleErr == nil {
		t.Fatalf("a superseded effect must be refused:\n%s", out)
	}
	// (c) ...while the effective effect recovers exactly once.
	effectPath := signRecoveryRecord(t, binary, repo, keyDir, map[string]any{
		"repo": "herdforge", "ref": "FAC-738", "candidate_sha": candidate,
		"base_sha": base, "branch": "main", "lease_id": "claim:584",
		"lease_generation": 2, "verdict": "REJECTED", "reviewer": "reviewer-fac738",
		"reviewer_family": "anthropic", "bus_sequence": 0, "canonical_body": deliveredBody,
	})
	out, effErr := runRecover(t, binary, repo, effectPath)
	if effErr != nil {
		t.Fatalf("the effective delivered effect must recover: %v\n%s", effErr, out)
	}
	entries, _ := os.ReadDir(filepath.Join(repo, ".herd", "review", "inbox"))
	if len(entries) != 1 {
		t.Fatalf("exactly one recovered artifact expected, got %d", len(entries))
	}
	// Idempotent: a second recovery of the same record converges.
	if _, err := runRecover(t, binary, repo, effectPath); err != nil {
		t.Fatalf("recovery must be idempotent: %v\n%s", err, out)
	}
	entries, _ = os.ReadDir(filepath.Join(repo, ".herd", "review", "inbox"))
	if len(entries) != 1 {
		t.Fatalf("recovery must not duplicate artifacts, got %d", len(entries))
	}
}

// TestSignAndRecoveryRefuseTrailingData proves both JSON surfaces are strict
// to end-of-input: a record followed by garbage (or another JSON value) is
// malformed evidence and must be refused, not signed or recovered (R3 round
// 5, finding 3).
func TestSignAndRecoveryRefuseTrailingData(t *testing.T) {
	binary := buildHerd(t)
	repo, candidate := corroborationRepo(t)
	baseOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD~1").Output()
	if err != nil {
		t.Fatal(err)
	}
	base := strings.TrimSpace(string(baseOut))

	keyDir := t.TempDir()
	if err := dispatch.WriteIsolationAttestation(keyDir, "test-sandbox"); err != nil {
		t.Fatal(err)
	}
	identity, idErr := dispatch.RepositoryIdentity(repo, "herdforge")
	if idErr != nil {
		t.Fatal(idErr)
	}
	signer, err := dispatch.LoadOrCreateSigner(keyDir, identity, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".herd", "herd.yaml"), []byte("version: \"1\"\nproject:\n  name: herdforge\ntask_provider:\n  type: memory\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := map[string]any{
		"repo": "herdforge", "ref": "FAC-741", "candidate_sha": candidate,
		"base_sha": base, "branch": "main", "lease_id": "claim:77",
		"lease_generation": 1, "verdict": "APPROVED", "reviewer": "reviewer-fac741",
		"reviewer_family": "openai", "bus_sequence": 0,
		"canonical_body": signDeliveredEffect(t, signer, "FAC-741", candidate, base, "claim:77", 1, "APPROVED", ""),
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}

	// (a) The signing surface refuses a record followed by another value.
	tampered := filepath.Join(t.TempDir(), "trailing.json")
	if err := os.WriteFile(tampered, append(raw, []byte(` {"later":true}`)...), 0o600); err != nil {
		t.Fatal(err)
	}
	signCmd := exec.Command(binary, "review-ingest", "--sign-recovery-record", tampered)
	signCmd.Dir = repo
	signCmd.Env = append(os.Environ(), "HERD_ROOT="+repo, "HERD_REPO_ROOT="+repo, "HERD_KEY_DIR="+keyDir)
	signOut, signErr := signCmd.CombinedOutput()
	if signErr == nil {
		t.Fatalf("signing must refuse a record with trailing data:\n%s", signOut)
	}
	if !strings.Contains(string(signOut), "trailing data") {
		t.Fatalf("refusal must be explicit about trailing data:\n%s", signOut)
	}

	// (b) The recovery surface refuses a signed record followed by garbage.
	signed := signRecoveryRecord(t, binary, repo, keyDir, rec)
	signedBody, err := os.ReadFile(signed)
	if err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(t.TempDir(), "signed-trailing.json")
	if err := os.WriteFile(corrupt, append(signedBody, []byte("\ngarbage after the record")...), 0o600); err != nil {
		t.Fatal(err)
	}
	out, recoverErr := runRecover(t, binary, repo, corrupt)
	if recoverErr == nil {
		t.Fatalf("recovery must refuse a record with trailing data:\n%s", out)
	}
	if !strings.Contains(out, "trailing data") {
		t.Fatalf("refusal must be explicit about trailing data:\n%s", out)
	}
	if entries, _ := os.ReadDir(filepath.Join(repo, ".herd", "review", "inbox")); len(entries) != 0 {
		t.Fatalf("refused input must publish nothing, got %d artifacts", len(entries))
	}
}
