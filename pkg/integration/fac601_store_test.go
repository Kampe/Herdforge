package integration

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFAC601StructurallyCorruptHistoryCannotResume(t *testing.T) {
	for _, name := range []string{"out-of-order", "duplicate", "wrong-candidate", "no-evidence", "invalid-time", "unknown-step"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			tx := drive(t, StepHarvest)
			switch name {
			case "out-of-order":
				tx.Done[0], tx.Done[1] = tx.Done[1], tx.Done[0]
			case "duplicate":
				tx.Done[1] = tx.Done[0]
			case "wrong-candidate":
				tx.Done[1].Candidate = strings.Repeat("f", 40)
			case "no-evidence":
				tx.Done[1].Evidence = " "
			case "invalid-time":
				tx.Done[1].RecordedAt = "yesterday"
			case "unknown-step":
				tx.Done[1].Step = "invented"
			}
			t.Setenv(StoreDirEnv, root)
			body, err := json.Marshal(tx)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(Path(root, cand), body, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(root, cand); err == nil {
				t.Fatal("corrupt ordered history admitted")
			}
			if err := os.Remove(Path(root, cand)); err != nil {
				t.Fatal(err)
			}
			if err := Save(root, tx); err == nil {
				t.Fatal("corrupt history persisted")
			}
		})
	}
}

func TestFAC601StaleWriterCannotEraseRecordedProgress(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	tx := drive(t, StepPass)
	if err := Save(root, tx); err != nil {
		t.Fatal(err)
	}
	stale, err := Load(root, cand)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Complete(StepHarvest, "harvest receipt"); err != nil {
		t.Fatal(err)
	}
	if err := Save(root, tx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(root, cand))
	if err != nil {
		t.Fatal(err)
	}
	if err := Save(root, stale); err == nil {
		t.Fatal("stale writer erased durable progress")
	}
	after, err := os.ReadFile(Path(root, cand))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("refused stale write changed history")
	}
}

func TestFAC601WriterCannotReplaceEarlierEvidence(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	tx := drive(t, StepPass)
	if err := Save(root, tx); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(root, cand))
	if err != nil {
		t.Fatal(err)
	}
	tx.Done[0].Evidence = "replacement assertion"
	if err := Save(root, tx); err == nil {
		t.Fatal("existing evidence replaced")
	}
	after, err := os.ReadFile(Path(root, cand))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("replacement changed history")
	}
}

func TestFAC601UnsafeCandidateCannotSelectStorePath(t *testing.T) {
	for _, candidate := range []string{"../../../../outside", strings.Repeat("z", 40), strings.Repeat("a", 41)} {
		if _, err := New(candidate); err == nil {
			t.Fatalf("non-SHA candidate accepted: %q", candidate)
		}
	}
}

func TestFAC601CanonicalCandidateResumesExistingHistory(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	tx := drive(t, StepHarvest)
	if err := Save(root, tx); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(root, "  "+cand+" \n")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Done) != 2 {
		t.Fatal("candidate whitespace hid existing history")
	}
	malformed := &Transaction{Candidate: " " + cand}
	if err := Save(root, malformed); err == nil {
		t.Fatal("noncanonical candidate persisted")
	}
}
