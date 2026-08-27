package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-710: an in-memory transaction dies with the process, which is most of
// what "transaction" is supposed to mean. A coordinator killed between merge
// and cleanup left no record of which steps had run, so the next operator could
// not tell a merged-but-uncleaned candidate from an unmerged one -- and the
// safe reading (assume nothing landed) is exactly how work gets done twice.

func TestTransactionSurvivesTheProcess(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(StoreDirEnv, dir)

	tx, err := New(cand)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []Step{StepPass, StepHarvest, StepIntegrationPR} {
		if err := tx.Complete(s, "evidence"); err != nil {
			t.Fatal(err)
		}
	}
	if err := Save(dir, tx); err != nil {
		t.Fatal(err)
	}

	reloaded, err := Load(dir, cand)
	if err != nil {
		t.Fatal(err)
	}
	next, _ := reloaded.Next()
	if next != StepMerge {
		t.Fatalf("resumed transaction lost its position: next=%s", next)
	}
	if len(reloaded.Done) != 3 {
		t.Fatalf("resumed transaction lost steps: %d", len(reloaded.Done))
	}
}

func TestMissingRecordIsANewTransactionNotAnError(t *testing.T) {
	// The first step of a candidate has nothing to resume.
	dir := t.TempDir()
	t.Setenv(StoreDirEnv, dir)
	tx, err := Load(dir, cand)
	if err != nil {
		t.Fatalf("a candidate with no prior record failed to start: %v", err)
	}
	if len(tx.Done) != 0 {
		t.Fatal("a fresh transaction arrived with history")
	}
}

func TestCorruptRecordRefusesRatherThanRestarting(t *testing.T) {
	// Silently restarting a lifecycle that may already have merged is exactly
	// the double-work this exists to prevent.
	dir := t.TempDir()
	t.Setenv(StoreDirEnv, dir)
	if err := os.WriteFile(filepath.Join(dir, short(cand)+".json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(dir, cand)
	if err == nil {
		t.Fatal("a corrupt record silently restarted the lifecycle")
	}
	if !strings.Contains(err.Error(), "may already have merged") {
		t.Fatalf("refusal does not name the risk: %v", err)
	}
}

func TestRecordForAnotherCandidateIsRefused(t *testing.T) {
	// Driving one candidate's lifecycle from another's receipt would land
	// content nobody reviewed.
	dir := t.TempDir()
	t.Setenv(StoreDirEnv, dir)
	other, err := New("ffffffffffffffffffffffffffffffffffffffff")
	if err != nil {
		t.Fatal(err)
	}
	// Write it under THIS candidate's filename.
	body := `{"candidate":"` + other.Candidate + `","done":[]}`
	if err := os.WriteFile(filepath.Join(dir, short(cand)+".json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir, cand); err == nil {
		t.Fatal("a receipt for a different candidate was accepted")
	}
}
