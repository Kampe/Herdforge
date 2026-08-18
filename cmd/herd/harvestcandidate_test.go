package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestResolveHarvestCandidatePinsReviewedSHAAcrossTipDrift(t *testing.T) {
	t.Chdir(t.TempDir())
	gitCandidateTest(t, "init", "-q", "-b", "main")
	gitCandidateTest(t, "config", "user.email", "test@example.com")
	gitCandidateTest(t, "config", "user.name", "test")
	gitCandidateTest(t, "commit", "--allow-empty", "-q", "-m", "base")
	gitCandidateTest(t, "branch", "standing/lane")
	gitCandidateTest(t, "checkout", "-q", "standing/lane")
	writeCandidateFile(t, "reviewed.go", "package reviewed\n")
	gitCandidateTest(t, "add", "reviewed.go")
	gitCandidateTest(t, "commit", "-q", "-m", "reviewed")
	reviewed := gitCandidateOutput(t, "rev-parse", "HEAD")

	writeCandidateFile(t, "advanced.go", "package advanced\n")
	gitCandidateTest(t, "add", "advanced.go")
	gitCandidateTest(t, "commit", "-q", "-m", "advanced")
	advanced := gitCandidateOutput(t, "rev-parse", "HEAD")

	l, err := reviewledger.NewReviewLedger(".", filepath.Join(".herd", "review-ledger.jsonl"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	addCandidatePass(t, l, reviewed, "standing/lane")

	tests := []struct {
		name      string
		requested string
		eligible  bool
		wantSHA   string
	}{
		{name: "moved tip refuses without new pass", eligible: false, wantSHA: ""},
		{name: "exact reviewed pin remains eligible", requested: reviewed, eligible: true, wantSHA: reviewed},
		{name: "advanced tip is not promoted", requested: advanced, eligible: false, wantSHA: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveHarvestCandidate("standing/lane", tt.requested)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Eligible != tt.eligible || got.Pin.SHA != tt.wantSHA {
				t.Fatalf("report = %+v, want eligible=%t pin=%q", got, tt.eligible, tt.wantSHA)
			}
			if got.Tip != advanced || got.LastPassSHA != reviewed {
				t.Fatalf("provenance = tip %q last pass %q, want %q and %q", got.Tip, got.LastPassSHA, advanced, reviewed)
			}
		})
	}

	addCandidatePass(t, l, advanced, "standing/lane")
	got, err := resolveHarvestCandidate("standing/lane", "")
	if err != nil {
		t.Fatalf("resolve after fresh pass: %v", err)
	}
	if !got.Eligible || got.Pin.SHA != advanced || got.LastPassSHA != advanced {
		t.Fatalf("fresh PASS did not admit exact new tip: %+v", got)
	}
	got, err = resolveHarvestCandidate("standing/lane", reviewed)
	if err != nil {
		t.Fatalf("resolve retained reviewed pin: %v", err)
	}
	if !got.Eligible || got.Pin.SHA != reviewed {
		t.Fatalf("exact reviewed pin lost eligibility after a later PASS: %+v", got)
	}
}

func addCandidatePass(t *testing.T, l *reviewledger.Ledger, sha, branch string) {
	t.Helper()
	if err := l.Record(reviewledger.RecordOpts{
		SHA: sha, Branch: branch, Reviewer: "reviewer", BuilderFamily: "anthropic",
		ReviewerFamily: "openai", Gate: "independent", Tier: "R2",
	}); err != nil {
		t.Fatalf("record: %v", err)
	}
	if _, err := l.Verdict(reviewledger.VerdictOpts{
		SHA: sha, Branch: branch, Reviewer: "reviewer", Verdict: reviewledger.VerdictPASS,
		ReviewerFamily: "openai", BuilderFamily: "anthropic",
	}); err != nil {
		t.Fatalf("verdict: %v", err)
	}
}

func writeCandidateFile(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

func gitCandidateTest(t *testing.T, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func gitCandidateOutput(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out))
}
