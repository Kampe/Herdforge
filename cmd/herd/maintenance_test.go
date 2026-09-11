package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type maintenanceTickCall struct {
	Root string
	Base string
	Act  bool
}

// stubMaintenanceTick replaces the cleanup beat and records what the carrier
// asked it to do.
func stubMaintenanceTick(t *testing.T, fn func(ctx context.Context, root, base string, act bool) (reapPulseReport, error)) *[]maintenanceTickCall {
	t.Helper()
	var mu sync.Mutex
	calls := &[]maintenanceTickCall{}
	original := maintenanceTick
	maintenanceTick = func(ctx context.Context, root, base string, act bool) (reapPulseReport, error) {
		mu.Lock()
		*calls = append(*calls, maintenanceTickCall{Root: root, Base: base, Act: act})
		mu.Unlock()
		return fn(ctx, root, base, act)
	}
	t.Cleanup(func() { maintenanceTick = original })
	return calls
}

// maintenanceScratchRoot points root resolution at a scratch checkout so no
// test ever touches a real repository's cursor or lock.
func maintenanceScratchRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	return root
}

// TestMaintenanceRunsWhileFleetAdmissionRefuses is the reason this command
// exists. The cleanup beat's existing callers run requireFleetAdmission first,
// so a wound-down repository stops cleaning up exactly when worktrees stop
// being consumed. The carrier must beat under the posture that refuses pulse.
func TestMaintenanceRunsWhileFleetAdmissionRefuses(t *testing.T) {
	maintenanceScratchRoot(t)
	state := filepath.Join(t.TempDir(), "winddown.json")
	body := `{"enabled":true,"actor":"test","reason":"posture closed","timestamp":"2026-09-11T00:00:00Z","generation":1}`
	if err := os.WriteFile(state, []byte(body), 0o600); err != nil {
		t.Fatalf("write winddown state: %v", err)
	}
	t.Setenv("HERD_WINDDOWN_STATE", state)

	// Without this the rest proves nothing: an open gate lets everything past.
	if err := requireFleetAdmission(context.Background()); err == nil {
		t.Fatal("fleet admission accepted a wound-down posture; this test can no longer prove independence")
	}

	calls := stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{Registered: 3, Eligible: 1}, nil
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), nil, &out, &errOut); code != 0 {
		t.Fatalf("maintenance exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if len(*calls) != 1 {
		t.Fatalf("cycles = %d, want 1 while admission refuses", len(*calls))
	}
}

// TestMaintenanceDefersToAHeldTickLock is the concurrent-exclusion proof, and
// it runs against the REAL lock rather than a stub: the carrier must share the
// pulse beat's kernel tick lock, not open a second one beside it. A carrier
// with its own fence would sweep straight through a live pulse beat.
func TestMaintenanceDefersToAHeldTickLock(t *testing.T) {
	root := maintenanceScratchRoot(t)

	held := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withReapPulseTickLock(root, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	select {
	case <-held:
	case <-time.After(10 * time.Second):
		t.Fatal("could not take the tick lock")
	}
	defer func() {
		close(release)
		<-lockDone
	}()

	// No stub: if exclusion is broken this reaches the real beat and the
	// failure is visible as a cycle line instead of a deferral.
	var out, errOut bytes.Buffer
	code := runMaintenanceCommandContext(context.Background(), []string{"--act"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("exit = %d, want 0: losing the tick-lock race is a deferral, not a fault; stderr=%q", code, errOut.String())
	}
	if !strings.Contains(errOut.String(), "deferred") {
		t.Fatalf("stderr %q does not report a deferral; the carrier did not share the pulse tick lock", errOut.String())
	}
	if strings.Contains(out.String(), "registered=") {
		t.Fatalf("stdout %q shows a completed cycle while another beat held the lock", out.String())
	}
}

// TestMaintenanceCancellationStopsBeforeAnotherCycle is the no-overlapping-
// mutation proof. The tick is cancellable only at its own checkpoints, so the
// carrier's obligation is to stop scheduling: once a cycle reports
// cancellation, no further cycle may begin.
//
// The two halves have different owners, verified by mutation: the exit code is
// this file's (removing the cancellation case makes an orderly stop exit 1),
// while "no second cycle" is daemon.RunPulseScheduler's own ctx.Done() check.
// Asserting both keeps the end-to-end contract pinned if either side changes.
func TestMaintenanceCancellationStopsBeforeAnotherCycle(t *testing.T) {
	maintenanceScratchRoot(t)
	ctx, cancel := context.WithCancel(context.Background())
	calls := stubMaintenanceTick(t, func(tickCtx context.Context, _, _ string, _ bool) (reapPulseReport, error) {
		cancel()
		return reapPulseReport{}, context.Canceled
	})

	var out, errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runMaintenanceCommandContext(ctx, []string{"--act", "--interval", "1ms", "--max-cycles", "5"}, &out, &errOut)
	}()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit = %d, want 0: an orderly stop is not a cleanup fault; stderr=%q", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("maintenance ignored its cancelled context")
	}
	if len(*calls) != 1 {
		t.Fatalf("cycles = %d, want 1: a cancelled cycle must not be followed by another", len(*calls))
	}
}

// TestMaintenanceReportsActualCycleCounts pins the bounded-cycle reporting:
// the operator sees what the beat really did, per cycle.
func TestMaintenanceReportsActualCycleCounts(t *testing.T) {
	maintenanceScratchRoot(t)
	stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{Registered: 41, Eligible: 12, Inspected: 8, Landed: 3, Retired: 2, Acted: true}, nil
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), []string{"--act", "--interval", "1ms", "--max-cycles", "2"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	for _, want := range []string{
		"cycle 1 registered=41 eligible=12 inspected=8 landed=3 retired=2 failed=0 acted=true",
		"cycle 2 registered=41",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("stdout %q missing %q", out.String(), want)
		}
	}
}

// TestMaintenanceResidentHonoursMaxCycles bounds the resident mode by its own
// contract, and the default runs exactly one cycle for a supervisor timer.
func TestMaintenanceResidentHonoursMaxCycles(t *testing.T) {
	for name, tc := range map[string]struct {
		args  []string
		want  int
		cycle string
	}{
		"default is one cycle": {args: nil, want: 1},
		"resident is bounded":  {args: []string{"--interval", "1ms", "--max-cycles", "3"}, want: 3},
	} {
		t.Run(name, func(t *testing.T) {
			maintenanceScratchRoot(t)
			calls := stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
				return reapPulseReport{}, nil
			})
			var out, errOut bytes.Buffer
			if code := runMaintenanceCommandContext(context.Background(), tc.args, &out, &errOut); code != 0 {
				t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
			}
			if len(*calls) != tc.want {
				t.Fatalf("cycles = %d, want %d", len(*calls), tc.want)
			}
		})
	}
}

// TestMaintenanceOneShotFailureExitsNonZero is the exit-contract proof. The
// scheduler records a tick error and keeps going, so a carrier that trusted it
// would report success on a failed cleanup.
func TestMaintenanceOneShotFailureExitsNonZero(t *testing.T) {
	maintenanceScratchRoot(t)
	stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{}, errors.New("git refused")
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), []string{"--act"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1 when the only cycle fails", code)
	}
	if !strings.Contains(errOut.String(), "git refused") {
		t.Fatalf("stderr %q loses the underlying failure", errOut.String())
	}
}

// TestMaintenanceInternalDeadlineUnderHealthyParentExitsNonZero closes the
// exit-contract hole: a tick can report Canceled or DeadlineExceeded from a
// context bound INSIDE itself while this process is perfectly healthy. Judging
// that by the error's identity reads a cleanup that never happened as an
// orderly stop and exits 0. Parent health is what separates the two.
func TestMaintenanceInternalDeadlineUnderHealthyParentExitsNonZero(t *testing.T) {
	for name, tickErr := range map[string]error{
		"internal deadline":     context.DeadlineExceeded,
		"internal cancellation": context.Canceled,
	} {
		t.Run(name, func(t *testing.T) {
			maintenanceScratchRoot(t)
			stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
				return reapPulseReport{}, tickErr
			})
			var out, errOut bytes.Buffer
			// context.Background(): the parent never shuts down, so nothing
			// here is an orderly stop.
			if code := runMaintenanceCommandContext(context.Background(), []string{"--act"}, &out, &errOut); code != 1 {
				t.Fatalf("exit = %d, want 1: %v under a healthy parent is a failed cycle; stderr=%q", code, tickErr, errOut.String())
			}
			// The exit code alone is also produced by the scheduler's own
			// result, so assert the fault path itself ran: the operator must be
			// told which cycle failed and how many did.
			for _, want := range []string{"cycle 1:", "1 of 1 cycle(s) failed"} {
				if !strings.Contains(errOut.String(), want) {
					t.Errorf("stderr %q missing %q: the cycle was not counted as a fault", errOut.String(), want)
				}
			}
		})
	}
}

// TestMaintenanceRetirementFailureExitsNonZero keeps a beat that could not
// retire what it selected from reading as success.
func TestMaintenanceRetirementFailureExitsNonZero(t *testing.T) {
	maintenanceScratchRoot(t)
	stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{Landed: 2, Retired: 1, Failed: 1, Acted: true}, nil
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), []string{"--act"}, &out, &errOut); code != 1 {
		t.Fatalf("exit = %d, want 1 when a retirement failed", code)
	}
}

// TestMaintenanceDeferralIsNotAFailure keeps the expected exclusion outcome
// off the fault path, so a busy repository does not look like a broken one.
func TestMaintenanceDeferralIsNotAFailure(t *testing.T) {
	maintenanceScratchRoot(t)
	stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{}, fmt.Errorf("worktree reap pulse: %w", errReapPulseTickBusy)
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), []string{"--act"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0 on a deferral; stderr=%q", code, errOut.String())
	}
}

// TestMaintenanceWithoutActNeverRetires guards the default: a mis-deployed
// supervisor job must observe, not remove.
func TestMaintenanceWithoutActNeverRetires(t *testing.T) {
	maintenanceScratchRoot(t)
	calls := stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{}, nil
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), nil, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	if (*calls)[0].Act {
		t.Fatal("the default cycle asked the beat to retire")
	}
}

// TestMaintenanceResolvesCanonicalRootAndConfiguredBase proves the carrier does
// not hardcode either. A configured default branch must reach the beat, so a
// repository that does not call its trunk "main" is classified against its own
// base rather than one that cannot resolve.
func TestMaintenanceResolvesCanonicalRootAndConfiguredBase(t *testing.T) {
	root := maintenanceScratchRoot(t)
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0o755); err != nil {
		t.Fatalf("prepare scratch root: %v", err)
	}
	cfg := "version: \"1\"\nproject:\n  name: scratch\n  default_branch: trunk\ntask_provider:\n  type: kaneo\n"
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	calls := stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
		return reapPulseReport{}, nil
	})
	var out, errOut bytes.Buffer
	if code := runMaintenanceCommandContext(context.Background(), []string{"--act"}, &out, &errOut); code != 0 {
		t.Fatalf("exit = %d, want 0; stderr=%q", code, errOut.String())
	}
	got := (*calls)[0]
	if got.Base != "origin/trunk" {
		t.Errorf("base = %q, want origin/trunk from the repository's own config", got.Base)
	}
	if got.Root != root {
		t.Errorf("root = %q, want the canonical %q", got.Root, root)
	}
}

func TestMaintenanceRejectsIncoherentFlags(t *testing.T) {
	for name, args := range map[string][]string{
		"max-cycles without interval": {"--max-cycles", "2"},
		"negative interval":           {"--interval", "-1s"},
		"negative max-cycles":         {"--interval", "1ms", "--max-cycles", "-1"},
		"stray positional":            {"retire"},
	} {
		t.Run(name, func(t *testing.T) {
			maintenanceScratchRoot(t)
			stubMaintenanceTick(t, func(context.Context, string, string, bool) (reapPulseReport, error) {
				t.Error("a cycle ran despite an invalid flag set")
				return reapPulseReport{}, nil
			})
			var out, errOut bytes.Buffer
			if code := runMaintenanceCommandContext(context.Background(), args, &out, &errOut); code != 2 {
				t.Fatalf("exit = %d, want 2 for %v", code, args)
			}
		})
	}
}

// TestValidateSupersedeInvocation pins the explicit opt-in contract: the
// replacement payload is mandatory and a drain can never be combined with it.
func TestValidateSupersedeInvocation(t *testing.T) {
	for name, tc := range map[string]struct {
		drain bool
		text  string
		want  string
	}{
		"drain refused":       {drain: true, text: "payload", want: "mutually exclusive"},
		"empty payload":       {text: "   ", want: "requires a replacement payload"},
		"valid":               {text: "retask: new payload", want: ""},
		"file payload counts": {text: "x", want: ""},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateSupersedeInvocation(tc.drain, tc.text)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("valid invocation refused: %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("err = %v, want substring %q", err, tc.want)
			}
		})
	}
}
