package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func activeFixture(observed *int) testBackend {
	return testBackend{observe: func(_ Transaction, i Intent) (Observation, error) {
		*observed++
		return Observation{Intent: i, State: EffectApplied, Evidence: "fixture observed effect"}, nil
	}, execute: func(Transaction, Intent) error { return errors.New("applied fixture must not execute") }}
}
func TestFAC601ActiveCandidateRetainedThroughCleanup(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	ctx := context.Background()
	calls := 0
	backend := activeFixture(&calls)
	nextCandidate := strings.Repeat("b", 40)
	for _, step := range Order {
		if _, err := AdvanceActive(ctx, root, cand, step, backend); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
		pending, err := PendingCandidate(root)
		if err != nil {
			t.Fatal(err)
		}
		if step == StepCleanup {
			if pending != "" {
				t.Fatal("completed candidate retained ownership")
			}
			break
		}
		if pending != cand {
			t.Fatal("unfinished candidate lost ownership")
		}
		before := calls
		if _, err := AdvanceActive(ctx, root, nextCandidate, StepPass, backend); err == nil {
			t.Fatalf("next candidate admitted before %s completed cycle", step)
		}
		if calls != before {
			t.Fatal("refused successor reached an effect observation")
		}
	}
	if _, err := AdvanceActive(ctx, root, nextCandidate, StepPass, backend); err != nil {
		t.Fatalf("successor refused after cleanup: %v", err)
	}
}
func TestFAC601ActiveAdmissionFailureDoesNotReserveCycle(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	calls := 0
	backend := activeFixture(&calls)
	backend.check = func(Transaction, Intent) error { return errors.New("admission refused") }
	if _, err := AdvanceActive(context.Background(), root, cand, StepPass, backend); err == nil {
		t.Fatal("admission failure accepted")
	}
	if _, err := os.Stat(activePath(root)); !os.IsNotExist(err) {
		t.Fatalf("failed admission reserved cycle: %v", err)
	}
	if calls != 0 {
		t.Fatal("failed admission reached effect")
	}
}
func TestFAC601ActiveDiscoveryRecoversOnlyUnambiguousManagedHistory(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	calls := 0
	backend := activeFixture(&calls)
	ctx := context.Background()
	if _, err := Advance(ctx, root, cand, StepPass, backend); err != nil {
		t.Fatal(err)
	}
	pending, err := PendingCandidate(root)
	if err != nil || pending != cand {
		t.Fatalf("lost pre-selector history: %s %v", pending, err)
	}
	other := strings.Repeat("b", 40)
	if _, err := AdvanceActive(ctx, root, other, StepPass, backend); err == nil {
		t.Fatal("unfinished legacy native cycle was ignored")
	}
	if _, err := Advance(ctx, root, other, StepPass, backend); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingCandidate(root); err == nil {
		t.Fatal("ambiguous native histories were treated as empty")
	}
}
func TestFAC601ActiveDiscoveryCorruptStateIsUnknown(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	if err := os.WriteFile(activePath(root), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingCandidate(root); err == nil {
		t.Fatal("corrupt selector treated as empty")
	}
	if err := os.Remove(activePath(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(root, cand), []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PendingCandidate(root); err == nil {
		t.Fatal("corrupt transaction treated as empty")
	}
}
func TestFAC601ActiveDiscoveryOfFreshRootDoesNotWrite(t *testing.T) {
	root := filepath.Join(t.TempDir(), "not-created")
	t.Setenv(StoreDirEnv, root)
	if candidate, err := PendingCandidate(root); err != nil || candidate != "" {
		t.Fatalf("fresh discovery: %s %v", candidate, err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("read-only discovery created state")
	}
}
