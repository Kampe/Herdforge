package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type testBackend struct {
	check   func(Transaction, Intent) error
	observe func(Transaction, Intent) (Observation, error)
	execute func(Transaction, Intent) error
}

func (b testBackend) Check(_ context.Context, tx Transaction, in Intent) error {
	if b.check != nil {
		return b.check(tx, in)
	}
	return nil
}
func (b testBackend) Observe(_ context.Context, tx Transaction, in Intent) (Observation, error) {
	return b.observe(tx, in)
}
func (b testBackend) Execute(_ context.Context, tx Transaction, in Intent) error {
	return b.execute(tx, in)
}

func fixtureBackend(t *testing.T, root string, calls *[]Step) testBackend {
	t.Helper()
	return testBackend{
		observe: func(_ Transaction, in Intent) (Observation, error) {
			b, err := os.ReadFile(filepath.Join(root, in.ID+".effect"))
			if os.IsNotExist(err) {
				return Observation{Intent: in, State: EffectAbsent}, nil
			}
			if err != nil {
				return Observation{}, err
			}
			return Observation{Intent: in, State: EffectApplied, Evidence: string(b)}, nil
		},
		execute: func(_ Transaction, in Intent) error {
			*calls = append(*calls, in.Step)
			// Read the actual durable file before every simulated side effect.
			tx, err := Load(root, cand)
			if err != nil {
				return err
			}
			if tx.Pending == nil || *tx.Pending != in {
				return errors.New("effect ran before durable intent")
			}
			return os.WriteFile(filepath.Join(root, in.ID+".effect"), []byte("observed "+string(in.Step)), 0600)
		},
	}
}

func TestFAC601AdvanceExactlyOneStepAndRetryDoesNotAdvance(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	var calls []Step
	backend := fixtureBackend(t, root, &calls)
	for index, step := range Order {
		r, err := Advance(context.Background(), root, cand, step, backend)
		if err != nil {
			t.Fatal(err)
		}
		if r.Step != step || r.Evidence != "observed "+string(step) {
			t.Fatalf("wrong record: %+v", r)
		}
		again, err := Advance(context.Background(), root, cand, step, backend)
		if err != nil || *again != *r {
			t.Fatalf("duplicate invocation: %+v %v", again, err)
		}
		tx, err := Load(root, cand)
		if err != nil {
			t.Fatal(err)
		}
		if len(tx.Done) != index+1 || len(calls) != index+1 || tx.Pending != nil {
			t.Fatalf("step %s advanced another effect: history=%d calls=%v pending=%+v", step, len(tx.Done), calls, tx.Pending)
		}
	}
}

func TestFAC601UnknownReadbackNeverExecutesAndRetainsIntent(t *testing.T) {
	for _, mode := range []string{"unknown", "transport-error", "wrong-candidate", "wrong-step", "wrong-operation", "empty-applied-evidence"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(StoreDirEnv, root)
			calls := 0
			backend := testBackend{
				observe: func(_ Transaction, in Intent) (Observation, error) {
					o := Observation{Intent: in, State: EffectApplied, Evidence: "real proof"}
					switch mode {
					case "unknown":
						o.State = EffectUnknown
					case "transport-error":
						return Observation{}, errors.New("provider timeout")
					case "wrong-candidate":
						o.Intent.Candidate = strings.Repeat("f", 40)
					case "wrong-step":
						o.Intent.Step = StepCleanup
					case "wrong-operation":
						o.Intent.ID = strings.Repeat("f", 32)
					case "empty-applied-evidence":
						o.Evidence = " "
					}
					return o, nil
				},
				execute: func(Transaction, Intent) error { calls++; return nil },
			}
			var first Intent
			for n := 0; n < 2; n++ {
				if _, err := Advance(context.Background(), root, cand, StepPass, backend); err == nil {
					t.Fatal("ambiguous result advanced")
				}
				tx, err := Load(root, cand)
				if err != nil {
					t.Fatal(err)
				}
				if tx.Pending == nil || len(tx.Done) != 0 || calls != 0 {
					t.Fatalf("lost intent or executed without readback: %+v calls=%d", tx, calls)
				}
				if n == 0 {
					first = *tx.Pending
				} else if first != *tx.Pending {
					t.Fatal("retry invented a new intent")
				}
			}
		})
	}
}

func TestFAC601ErrorAfterEffectReconcilesWithoutRepeating(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	var calls []Step
	backend := fixtureBackend(t, root, &calls)
	execute := backend.execute
	backend.execute = func(tx Transaction, in Intent) error {
		if err := execute(tx, in); err != nil {
			return err
		}
		return errors.New("connection lost after effect")
	}
	if _, err := Advance(context.Background(), root, cand, StepPass, backend); err == nil {
		t.Fatal("ambiguous execution succeeded")
	}
	if _, err := Advance(context.Background(), root, cand, StepPass, backend); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("repeated applied effect: %v", calls)
	}
}

func TestFAC601ProcessDeathAfterEffectResumesWithoutExecute(t *testing.T) {
	if root := os.Getenv("FAC601_DRIVER_CRASH_FIXTURE"); root != "" {
		var calls []Step
		backend := fixtureBackend(t, root, &calls)
		execute := backend.execute
		backend.execute = func(tx Transaction, in Intent) error {
			if err := execute(tx, in); err != nil {
				return err
			}
			os.Exit(23)
			return nil
		}
		_, err := Advance(context.Background(), root, cand, StepPass, backend)
		t.Fatalf("crash fixture did not exit: %v", err)
	}
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestFAC601ProcessDeathAfterEffectResumesWithoutExecute$")
	cmd.Env = append(os.Environ(), "FAC601_DRIVER_CRASH_FIXTURE="+root)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 23 {
		t.Fatalf("wrong child exit: %v %s", err, output)
	}
	tx, err := Load(root, cand)
	if err != nil {
		t.Fatal(err)
	}
	if tx.Pending == nil || len(tx.Done) != 0 {
		t.Fatalf("missing crash recovery intent: %+v", tx)
	}
	var calls []Step
	if _, err := Advance(context.Background(), root, cand, StepPass, fixtureBackend(t, root, &calls)); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatal("crash recovery repeated an externally completed effect")
	}
}

func TestFAC601ConcurrentSameStepExecutesOnce(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	var calls []Step
	backend := fixtureBackend(t, root, &calls)
	var wg sync.WaitGroup
	errorsCh := make(chan error, 2)
	for n := 0; n < 2; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := Advance(context.Background(), root, cand, StepPass, backend)
			errorsCh <- err
		}()
	}
	wg.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(calls) != 1 {
		t.Fatalf("parallel calls performed %d effects", len(calls))
	}
}

func TestFAC601AdmissionAndOrderRefusalsHaveNoEffects(t *testing.T) {
	for _, mode := range []string{"gate", "cleanup", "merge", "cancelled", "short-sha", "manual-history"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv(StoreDirEnv, root)
			var calls []Step
			backend := fixtureBackend(t, root, &calls)
			ctx := context.Background()
			candidate := cand
			step := StepPass
			switch mode {
			case "gate":
				backend.check = func(Transaction, Intent) error { return errors.New("live review admission refused") }
			case "cleanup":
				step = StepCleanup
			case "merge":
				step = StepMerge
			case "cancelled":
				cancelCtx, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelCtx
			case "short-sha":
				candidate = cand[:12]
			case "manual-history":
				if err := Save(root, drive(t, StepPass)); err != nil {
					t.Fatal(err)
				}
				step = StepHarvest
			}
			if _, err := Advance(ctx, root, candidate, step, backend); err == nil {
				t.Fatal("invalid execution admitted")
			}
			if len(calls) != 0 {
				t.Fatal("refusal executed effects")
			}
			if mode != "manual-history" {
				if _, err := os.Stat(Path(root, cand)); !os.IsNotExist(err) {
					t.Fatalf("refusal wrote transaction: %v", err)
				}
			}
		})
	}
}

func TestFAC601ManualCompletionCannotBypassExecutedSteps(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	var calls []Step
	if _, err := Advance(context.Background(), root, cand, StepPass, fixtureBackend(t, root, &calls)); err != nil {
		t.Fatal(err)
	}
	tx, err := Load(root, cand)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Complete(StepHarvest, "operator assertion"); err == nil {
		t.Fatal("manual completion bypassed native effect")
	}
	if err := Save(root, tx); err == nil {
		t.Fatal("manual Save accepted managed transaction")
	}
	tx.DriverVersion = 0
	if err := tx.Complete(StepHarvest, "operator assertion"); err != nil {
		t.Fatal(err)
	}
	if err := Save(root, tx); err == nil {
		t.Fatal("manual downgrade erased execution fence")
	}
}

func TestFAC601BackendCannotMutateDurableHistory(t *testing.T) {
	root := t.TempDir()
	t.Setenv(StoreDirEnv, root)
	var calls []Step
	backend := fixtureBackend(t, root, &calls)
	if _, err := Advance(context.Background(), root, cand, StepPass, backend); err != nil {
		t.Fatal(err)
	}
	backend.check = func(tx Transaction, in Intent) error {
		tx.Done[0].Evidence = "tampered"
		if tx.Pending != nil {
			tx.Pending.ID = "tampered"
		}
		return nil
	}
	if _, err := Advance(context.Background(), root, cand, StepHarvest, backend); err != nil {
		t.Fatal(err)
	}
	tx, err := Load(root, cand)
	if err != nil {
		t.Fatal(err)
	}
	if got := tx.Done[0].Evidence; got != "observed "+string(StepPass) {
		t.Fatal(fmt.Sprintf("backend modified history: %s", got))
	}
}
