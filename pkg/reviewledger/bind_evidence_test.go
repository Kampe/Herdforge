package reviewledger

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/verifier"
)

const (
	fac618SHA       = "46be267dd2cc0a42acb70141838d0e3f5645605b"
	fac618TS        = "2026-09-05T23:32:46.874718Z"
	fac618Reviewer  = "pool-02"
	decoyBuildA     = "1bcf5266bf1b0000000000000000000000000000"
	decoyBuildB     = "89d2b1c6f0250000000000000000000000000000"
	decoyBlockedBld = "a7006e398e470000000000000000000000000000"
	decoyBlockedSt  = "c491868348d20000000000000000000000000000"
	wrongSHA        = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

var (
	fullSuiteArgv = []string{"go", "test", "-timeout=30m0s", "./..."}
	buildOnlyArgv = []string{"go", "build", "./..."}
)

func authenticReceipt(t *testing.T, task, sha, lease string, outcome verifier.Outcome, argv []string) verifier.Receipt {
	t.Helper()
	r := verifier.Receipt{
		Version:           1,
		TaskRef:           task,
		LeaseGeneration:   lease,
		CandidateSHA:      sha,
		Command:           append([]string(nil), argv...),
		EnvironmentPolicy: verifier.EnvironmentPolicyInherited,
		Outcome:           outcome,
	}
	if outcome != verifier.OutcomePASS {
		r.ExitCode = 1
	}
	r.Digest = r.ComputeDigest()
	if err := r.ValidateDigest(); err != nil {
		t.Fatalf("fixture receipt digest: %v", err)
	}
	return r
}

func rawLedger(t *testing.T, lines ...string) (*Ledger, []byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "review-ledger.jsonl")
	body := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := NewReviewLedger(dir, path)
	if err != nil {
		t.Fatal(err)
	}
	l.Now = fixedNow
	return l, []byte(body)
}

func independentPair(sha, task, ts string) (string, string) {
	record, _ := json.Marshal(map[string]any{
		"ts": "2026-09-05T23:30:00.000000Z", "event": "record", "sha": sha,
		"reviewer": fac618Reviewer, "builder_family": "openai", "builder_identity": "builder-openai",
		"reviewer_family": "anthropic", "tier": "R2", "task": task,
	})
	verdict, _ := json.Marshal(map[string]any{
		"ts": ts, "event": "verdict", "sha": sha, "reviewer": fac618Reviewer,
		"verdict": "PASS", "builder_family": "openai", "reviewer_family": "anthropic",
		"task": task,
	})
	return string(record), string(verdict)
}

func TestBindEvidence_FAC618AdmitsExactPairAndPreservesHistoricalBytes(t *testing.T) {
	record, verdict := independentPair(fac618SHA, "FAC-618", fac618TS)
	l, before := rawLedger(t, record, verdict)
	receipt := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)

	if err := l.BindEvidence("FAC-618", fac618SHA, receipt.Digest, receipt); err != nil {
		t.Fatalf("bind exact pair: %v", err)
	}
	after, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Fatalf("historical rows were rewritten; want original bytes preserved as prefix")
	}
	if bytes.Equal(after, before) {
		t.Fatal("expected an append-only evidence-bind event")
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 || rows[2].Event != string(EventEvidenceBind) {
		t.Fatalf("rows=%d last=%q, want evidence-bind append", len(rows), rows[len(rows)-1].Event)
	}
	if rows[1].Timestamp != fac618TS || rows[1].Verdict != string(VerdictPASS) || rows[1].Reviewer != fac618Reviewer || rows[1].VerificationDigest != "" {
		t.Fatalf("original verdict mutated: %+v", rows[1])
	}
	if rows[2].VerificationDigest != receipt.Digest || rows[2].SHA != fac618SHA || rows[2].Task != "FAC-618" || rows[2].Reviewer != fac618Reviewer {
		t.Fatalf("bind event %+v", rows[2])
	}

	res, err := l.AdmitReduced(ReducedAdmissionOpts{CandidateSHA: fac618SHA})
	if err != nil && (res == nil || !res.Admitted) {
		t.Fatalf("admission after bind: admitted=%v err=%v reason=%q", res != nil && res.Admitted, err, res.Reason)
	}
	if !res.Admitted {
		t.Fatalf("FAC-618 exact pair must admit, reason=%q", res.Reason)
	}
	if res.VerificationDigest != receipt.Digest {
		t.Fatalf("admitted digest=%q want bound %q", res.VerificationDigest, receipt.Digest)
	}
	if res.Reviewer != fac618Reviewer || res.ReviewerFamily != "anthropic" || res.AuthorFamily != "openai" {
		t.Fatalf("admission changed reviewer provenance: %+v", res)
	}
	again, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, again) {
		t.Fatal("admission rewrote ledger rows")
	}
}

func TestBindEvidence_IdenticalRetryIsIdempotent(t *testing.T) {
	record, verdict := independentPair(fac618SHA, "FAC-618", fac618TS)
	l, _ := rawLedger(t, record, verdict)
	receipt := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)
	if err := l.BindEvidence("FAC-618", fac618SHA, receipt.Digest, receipt); err != nil {
		t.Fatal(err)
	}
	mid, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.BindEvidence("FAC-618", fac618SHA, receipt.Digest, receipt); err != nil {
		t.Fatalf("identical retry: %v", err)
	}
	after, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mid, after) {
		t.Fatal("identical bind retry wrote again")
	}
}

func TestBindEvidence_RefusesWithoutWrites(t *testing.T) {
	record, verdict := independentPair(fac618SHA, "FAC-618", fac618TS)
	buildARec, buildAVer := independentPair(decoyBuildA, "FAC-618", fac618TS)
	buildBRec, buildBVer := independentPair(decoyBuildB, "FAC-618", fac618TS)
	blkBRec, blkBVer := independentPair(decoyBlockedBld, "FAC-618", fac618TS)
	blkSRec, blkSVer := independentPair(decoyBlockedSt, "FAC-618", fac618TS)

	good := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)
	buildA := authenticReceipt(t, "FAC-618", decoyBuildA, "2", verifier.OutcomePASS, buildOnlyArgv)
	buildB := authenticReceipt(t, "FAC-618", decoyBuildB, "2", verifier.OutcomePASS, buildOnlyArgv)
	blockedBuild := authenticReceipt(t, "FAC-618", decoyBlockedBld, "2", verifier.OutcomeBLOCKED, buildOnlyArgv)
	blockedSuite := authenticReceipt(t, "FAC-618", decoyBlockedSt, "2", verifier.OutcomeBLOCKED, fullSuiteArgv)
	wrongTask := authenticReceipt(t, "FAC-999", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)
	wrongCandidate := authenticReceipt(t, "FAC-618", wrongSHA, "2", verifier.OutcomePASS, fullSuiteArgv)
	pkgScoped := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, []string{"go", "test", "./pkg/reviewledger"})
	failSuite := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomeFAIL, fullSuiteArgv)

	fabricated := good
	fabricated.Digest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"

	type tc struct {
		name    string
		lines   []string
		task    string
		sha     string
		digest  string
		receipt verifier.Receipt
		want    string
	}
	cases := []tc{
		{name: "wrong ref", lines: []string{record, verdict}, task: "FAC-999", sha: fac618SHA, digest: good.Digest, receipt: good, want: "task"},
		{name: "wrong SHA", lines: []string{record, verdict}, task: "FAC-618", sha: wrongSHA, digest: good.Digest, receipt: good, want: "candidate"},
		{name: "receipt task mismatch", lines: []string{record, verdict}, task: "FAC-618", sha: fac618SHA, digest: wrongTask.Digest, receipt: wrongTask, want: "task"},
		{name: "receipt SHA mismatch", lines: []string{record, verdict}, task: "FAC-618", sha: fac618SHA, digest: wrongCandidate.Digest, receipt: wrongCandidate, want: "candidate"},
		{name: "build-only decoy 1bcf5266bf1b", lines: []string{buildARec, buildAVer}, task: "FAC-618", sha: decoyBuildA, digest: buildA.Digest, receipt: buildA, want: "full-suite"},
		{name: "build-only decoy 89d2b1c6f025", lines: []string{buildBRec, buildBVer}, task: "FAC-618", sha: decoyBuildB, digest: buildB.Digest, receipt: buildB, want: "full-suite"},
		{name: "BLOCKED build a7006e398e47", lines: []string{blkBRec, blkBVer}, task: "FAC-618", sha: decoyBlockedBld, digest: blockedBuild.Digest, receipt: blockedBuild, want: "PASS"},
		{name: "BLOCKED suite c491868348d2", lines: []string{blkSRec, blkSVer}, task: "FAC-618", sha: decoyBlockedSt, digest: blockedSuite.Digest, receipt: blockedSuite, want: "PASS"},
		{name: "FAIL full-suite", lines: []string{record, verdict}, task: "FAC-618", sha: fac618SHA, digest: failSuite.Digest, receipt: failSuite, want: "PASS"},
		{name: "package-scoped test is not full-suite", lines: []string{record, verdict}, task: "FAC-618", sha: fac618SHA, digest: pkgScoped.Digest, receipt: pkgScoped, want: "full-suite"},
		{name: "fabricated digest", lines: []string{record, verdict}, task: "FAC-618", sha: fac618SHA, digest: fabricated.Digest, receipt: fabricated, want: "digest"},
		{name: "absent PASS", lines: []string{record}, task: "FAC-618", sha: fac618SHA, digest: good.Digest, receipt: good, want: "no independent PASS"},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			l, before := rawLedger(t, c.lines...)
			err := l.BindEvidence(c.task, c.sha, c.digest, c.receipt)
			if err == nil {
				t.Fatal("expected refusal")
			}
			if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(c.want)) {
				t.Fatalf("error %q does not name %q", err, c.want)
			}
			after, readErr := os.ReadFile(l.Path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(before, after) {
				t.Fatalf("refusal wrote to the ledger: %s", after)
			}
		})
	}
}

func TestBindEvidence_AmbiguousAndContradictoryRefuse(t *testing.T) {
	record1, verdict1 := independentPair(fac618SHA, "FAC-618", fac618TS)
	second, _ := json.Marshal(map[string]any{
		"ts": "2026-09-05T23:33:00.000000Z", "event": "record", "sha": fac618SHA,
		"reviewer": "pool-03", "builder_family": "openai", "builder_identity": "builder-openai",
		"reviewer_family": "google", "tier": "R2", "task": "FAC-618",
	})
	secondPass, _ := json.Marshal(map[string]any{
		"ts": "2026-09-05T23:33:01.000000Z", "event": "verdict", "sha": fac618SHA, "reviewer": "pool-03",
		"verdict": "PASS", "builder_family": "openai", "reviewer_family": "google", "task": "FAC-618",
	})
	fail, _ := json.Marshal(map[string]any{
		"ts": "2026-09-05T23:33:02.000000Z", "event": "verdict", "sha": fac618SHA, "reviewer": "pool-03",
		"verdict": "FAIL", "builder_family": "openai", "reviewer_family": "google", "task": "FAC-618",
	})
	receipt := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)

	t.Run("ambiguous PASS", func(t *testing.T) {
		l, before := rawLedger(t, record1, verdict1, string(second), string(secondPass))
		err := l.BindEvidence("FAC-618", fac618SHA, receipt.Digest, receipt)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "ambiguous") {
			t.Fatalf("want ambiguous refusal, got %v", err)
		}
		after, _ := os.ReadFile(l.Path)
		if !bytes.Equal(before, after) {
			t.Fatal("ambiguous refusal wrote")
		}
	})
	t.Run("contradictory FAIL", func(t *testing.T) {
		l, before := rawLedger(t, record1, verdict1, string(second), string(fail))
		err := l.BindEvidence("FAC-618", fac618SHA, receipt.Digest, receipt)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "contradict") {
			t.Fatalf("want contradictory refusal, got %v", err)
		}
		after, _ := os.ReadFile(l.Path)
		if !bytes.Equal(before, after) {
			t.Fatal("contradictory refusal wrote")
		}
	})
}

func TestBindEvidence_ConflictingBindingFailsClosed(t *testing.T) {
	record, verdict := independentPair(fac618SHA, "FAC-618", fac618TS)
	l, _ := rawLedger(t, record, verdict)
	first := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)
	if err := l.BindEvidence("FAC-618", fac618SHA, first.Digest, first); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	second := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, []string{"go", "test", "-timeout=30m0s", "./...", "-count=1"})
	err = l.BindEvidence("FAC-618", fac618SHA, second.Digest, second)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "conflict") {
		t.Fatalf("want conflicting binding refusal, got %v", err)
	}
	after, err := os.ReadFile(l.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("conflicting bind wrote")
	}
}

func TestBindEvidence_DoesNotEnqueueOrConsume(t *testing.T) {
	record, verdict := independentPair(fac618SHA, "FAC-618", fac618TS)
	l, _ := rawLedger(t, record, verdict)
	receipt := authenticReceipt(t, "FAC-618", fac618SHA, "2", verifier.OutcomePASS, fullSuiteArgv)
	if err := l.BindEvidence("FAC-618", fac618SHA, receipt.Digest, receipt); err != nil {
		t.Fatal(err)
	}
	q, err := l.QueueRows()
	if err != nil {
		t.Fatal(err)
	}
	if len(q) != 0 {
		t.Fatalf("binding must not write harvest queue rows: %+v", q)
	}
	rows, err := l.AllRows()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Event == string(EventConsumed) || row.Event == string(EventEnqueue) || row.Event == string(EventVerdict) && row.Timestamp != fac618TS {
			t.Fatalf("binding mutated authority events: %+v", row)
		}
	}
}
