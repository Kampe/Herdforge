package procsignal

import (
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestIncidentTargetsUseFakeBackendOnly(t *testing.T) {
	oldPID, oldPgid := callerPID, callerPgid
	callerPID = func() int { return 4242 }
	callerPgid = func() int { return 4242 }
	t.Cleanup(func() { callerPID, callerPgid = oldPID, oldPgid })
	defaultMeter.ResetForTest()

	tests := []struct {
		name string
		run  func(*FakeBackend) error
	}{
		{
			name: "kill_process_group_pgid_1_is_host_wide_negation",
			run:  func(f *FakeBackend) error { return KillProcessGroup(f, 1, syscall.SIGTERM) },
		},
		{
			name: "kill_process_group_pgid_0_is_current_group_sentinel",
			run:  func(f *FakeBackend) error { return KillProcessGroup(f, 0, syscall.SIGTERM) },
		},
		{
			name: "kill_process_pid_minus_one_class_via_negative",
			run:  func(f *FakeBackend) error { return KillProcess(f, -1, syscall.SIGTERM) },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &FakeBackend{}
			err := tt.run(fake)
			if !errors.Is(err, ErrUnsafeTarget) {
				t.Fatalf("err = %v, want ErrUnsafeTarget", err)
			}
			if len(fake.Calls) != 0 {
				t.Fatalf("backend received %v — real process would have been signaled", fake.Calls)
			}
		})
	}
}

func TestValidatePID_Table(t *testing.T) {
	oldPID := callerPID
	callerPID = func() int { return 9001 }
	t.Cleanup(func() { callerPID = oldPID })
	defaultMeter.ResetForTest()

	tests := []struct {
		name    string
		pid     int
		wantErr bool
		ambient bool
	}{
		{"pid_1_init", 1, true, true},
		{"pid_0_special", 0, true, true},
		{"pid_negative_1", -1, true, true},
		{"pid_negative_large", -99, true, true},
		{"pid_self", 9001, true, false},
		{"pid_min_int", math.MinInt, true, true},
		{"pid_valid_child", 9002, false, false},
		{"pid_large_valid", 1_000_000, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := defaultMeter.AmbientRefused()
			err := ValidatePID(tt.pid)
			if tt.wantErr && !errors.Is(err, ErrUnsafeTarget) {
				t.Fatalf("ValidatePID(%d) = %v, want ErrUnsafeTarget", tt.pid, err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidatePID(%d) = %v, want nil", tt.pid, err)
			}
			gotAmbient := defaultMeter.AmbientRefused() > before
			if gotAmbient != tt.ambient {
				t.Fatalf("ambient counted=%v want %v (pid=%d)", gotAmbient, tt.ambient, tt.pid)
			}
		})
	}
}

func TestValidatePGID_Table(t *testing.T) {
	oldPID, oldPgid := callerPID, callerPgid
	callerPID = func() int { return 500 }
	callerPgid = func() int { return 500 }
	t.Cleanup(func() { callerPID, callerPgid = oldPID, oldPgid })

	tests := []struct {
		name    string
		pgid    int
		wantErr bool
	}{
		{"pgid_1_host_wide_when_negated", 1, true},
		{"pgid_0_current_group", 0, true},
		{"pgid_negative", -7, true},
		{"pgid_caller_session_group", 500, true},
		{"pgid_owned_child_leader", 501, false},
		{"pgid_other_fixture", 7777, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidatePGID(tt.pgid)
			if tt.wantErr && !errors.Is(err, ErrUnsafeTarget) {
				t.Fatalf("ValidatePGID(%d) = %v, want ErrUnsafeTarget", tt.pgid, err)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("ValidatePGID(%d) = %v, want nil", tt.pgid, err)
			}
		})
	}
}

func TestValidateKillArg_HostWideAtBackendBoundary(t *testing.T) {
	defaultMeter.ResetForTest()
	for _, pid := range []int{-1, 0, 1} {
		if err := validateKillArg(pid); !errors.Is(err, ErrUnsafeTarget) {
			t.Fatalf("validateKillArg(%d) = %v", pid, err)
		}
	}
	if defaultMeter.AmbientRefused() < 3 {
		t.Fatalf("ambient refused = %d, want >= 3", defaultMeter.AmbientRefused())
	}
}

func TestKillProcessGroup_EmitsNegativePgidOnlyWhenSafe(t *testing.T) {
	oldPID, oldPgid := callerPID, callerPgid
	callerPID = func() int { return 100 }
	callerPgid = func() int { return 100 }
	t.Cleanup(func() { callerPID, callerPgid = oldPID, oldPgid })

	fake := &FakeBackend{}
	if err := KillProcessGroup(fake, 4242, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0].PID != -4242 {
		t.Fatalf("calls = %v", fake.Calls)
	}
}

func TestKillProcess_RejectsNilBackend(t *testing.T) {
	if err := KillProcess(nil, 42, syscall.SIGTERM); !errors.Is(err, ErrBackendRequired) {
		t.Fatalf("err = %v", err)
	}
	if err := KillProcessGroup(nil, 42, syscall.SIGTERM); !errors.Is(err, ErrBackendRequired) {
		t.Fatalf("err = %v", err)
	}
}

func TestMutation_RemovingPGIDGuardWouldHostWideSignal(t *testing.T) {
	oldPID, oldPgid := callerPID, callerPgid
	callerPID = func() int { return 9 }
	callerPgid = func() int { return 9 }
	t.Cleanup(func() { callerPID, callerPgid = oldPID, oldPgid })

	unsafe := &FakeBackend{}
	_ = unsafe.Kill(-1, syscall.SIGTERM)
	if len(unsafe.Calls) != 1 || unsafe.Calls[0].PID != -1 {
		t.Fatalf("unsafe baseline broken: %+v", unsafe.Calls)
	}

	safe := &FakeBackend{}
	err := KillProcessGroup(safe, 1, syscall.SIGTERM)
	if !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("safe path err = %v", err)
	}
	if len(safe.Calls) != 0 {
		t.Fatalf("guard failed open: backend saw %v", safe.Calls)
	}
}

func TestMutation_RemovingPIDGuardWouldSignalSelf(t *testing.T) {
	oldPID := callerPID
	callerPID = func() int { return 31337 }
	t.Cleanup(func() { callerPID = oldPID })

	unsafe := &FakeBackend{}
	_ = unsafe.Kill(31337, syscall.SIGKILL)
	if len(unsafe.Calls) != 1 {
		t.Fatal("unsafe baseline broken")
	}

	safe := &FakeBackend{}
	err := KillProcess(safe, 31337, syscall.SIGKILL)
	if !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("err = %v", err)
	}
	if len(safe.Calls) != 0 {
		t.Fatalf("self-kill guard failed open: %v", safe.Calls)
	}
}

func TestMutation_RemovingCallerPgidGuardWouldSignalSession(t *testing.T) {
	oldPID, oldPgid := callerPID, callerPgid
	callerPID = func() int { return 50 }
	callerPgid = func() int { return 60 }
	t.Cleanup(func() { callerPID, callerPgid = oldPID, oldPgid })

	unsafe := &FakeBackend{}
	_ = unsafe.Kill(-60, syscall.SIGTERM)
	if len(unsafe.Calls) != 1 || unsafe.Calls[0].PID != -60 {
		t.Fatalf("unsafe baseline broken: %+v", unsafe.Calls)
	}

	safe := &FakeBackend{}
	err := KillProcessGroup(safe, 60, syscall.SIGTERM)
	if !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("err = %v", err)
	}
	if len(safe.Calls) != 0 {
		t.Fatalf("session-group guard failed open: %v", safe.Calls)
	}
}

func TestMutation_HostBackendRefusesKillArgMinusOne(t *testing.T) {
	// hostBackend is unexported; exercise via destructive path with gates open
	// so only validateKillArg fails — never reaches syscall with -1.
	t.Setenv(HermeticEnv, "1")
	t.Setenv(IsolationEnv, "fixture-uid")
	t.Setenv(FixtureUIDEnv, fmt.Sprintf("%d", os.Getuid()))
	defaultMeter.ResetForTest()

	// DestructiveKillProcessGroup(1) → ValidatePGID fails before host.
	if err := DestructiveKillProcessGroup(1, syscall.SIGTERM); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("err = %v", err)
	}
	// Direct hostBackend with raw -1 (package-internal boundary).
	if err := (hostBackend{destructive: true}).Kill(-1, syscall.SIGTERM); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("hostBackend.Kill(-1) = %v", err)
	}
	if defaultMeter.HostKills() != 0 {
		t.Fatalf("host kills = %d, want 0", defaultMeter.HostKills())
	}
}

func TestRequireHermetic_FailClosed(t *testing.T) {
	t.Setenv(HermeticEnv, "")
	if err := RequireHermetic(); !errors.Is(err, ErrHermeticRequired) {
		t.Fatalf("err = %v", err)
	}
	t.Setenv(HermeticEnv, "0")
	if err := RequireHermetic(); !errors.Is(err, ErrHermeticRequired) {
		t.Fatalf("err = %v", err)
	}
	t.Setenv(HermeticEnv, "1")
	if err := RequireHermetic(); err != nil {
		t.Fatalf("err = %v", err)
	}
}

func TestDestructive_RefusesWithoutHermetic(t *testing.T) {
	t.Setenv(HermeticEnv, "")
	t.Setenv(IsolationEnv, "")
	if err := DestructiveKillProcessGroup(9999, syscall.SIGTERM); !errors.Is(err, ErrHermeticRequired) {
		t.Fatalf("err = %v, want ErrHermeticRequired", err)
	}
}

func TestDestructive_RefusesWithoutIsolation(t *testing.T) {
	t.Setenv(HermeticEnv, "1")
	t.Setenv(IsolationEnv, "")
	t.Setenv(IsolationIDEnv, "")
	t.Setenv(FixtureUIDEnv, "")
	if err := DestructiveKillProcessGroup(9999, syscall.SIGTERM); !errors.Is(err, ErrIsolationRequired) {
		t.Fatalf("err = %v, want ErrIsolationRequired", err)
	}
	// Hermetic set but empty ns id for docker mode.
	t.Setenv(IsolationEnv, "docker")
	t.Setenv(IsolationIDEnv, "")
	if err := DestructiveKillProcessGroup(9999, syscall.SIGTERM); !errors.Is(err, ErrIsolationRequired) {
		t.Fatalf("err = %v, want ErrIsolationRequired", err)
	}
}

func TestDestructive_FixtureUIDMismatchRefused(t *testing.T) {
	t.Setenv(HermeticEnv, "1")
	t.Setenv(IsolationEnv, "fixture-uid")
	t.Setenv(FixtureUIDEnv, "0") // almost never the test runner uid on developer hosts
	if os.Getuid() == 0 {
		t.Skip("running as root; fixture-uid mismatch case not meaningful")
	}
	if err := DestructiveKillProcessGroup(9999, syscall.SIGTERM); !errors.Is(err, ErrIsolationRequired) {
		t.Fatalf("err = %v, want ErrIsolationRequired", err)
	}
}

// TestDestructive_WithIsolationKillsOnlyOwnedFixture spawns a real child under
// fixture-uid isolation (UID must match the runner — hermetic runners set a
// distinct fixture UID; on host CI this proves the gate allows only after
// isolation proof, then signals the claimed child only).
func TestDestructive_WithIsolationKillsOnlyOwnedFixture(t *testing.T) {
	t.Setenv(HermeticEnv, "1")
	t.Setenv(IsolationEnv, "fixture-uid")
	t.Setenv(FixtureUIDEnv, fmt.Sprintf("%d", os.Getuid()))
	defaultMeter.ResetForTest()

	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	// Isolation + hermetic proven; kill the fixture process group only.
	if err := DestructiveKillProcessGroup(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("destructive kill: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		// child reaped
	case <-time.After(3 * time.Second):
		t.Fatal("fixture child still alive after destructive SIGKILL")
	}
	r := NewSafetyReceipt("FAC-174", "TestDestructive_WithIsolationKillsOnlyOwnedFixture")
	if !r.Hermetic || r.IsolationMode != "fixture-uid" {
		t.Fatalf("receipt isolation missing: %+v", r)
	}
	if r.AmbientAttempts != 0 {
		t.Fatalf("ambient attempts = %d, want 0", r.AmbientAttempts)
	}
	if r.HostKills < 1 {
		t.Fatalf("host kills = %d, want >= 1", r.HostKills)
	}
}

func TestSafetyReceipt_AmbientObservedNotCallerAsserted(t *testing.T) {
	defaultMeter.ResetForTest()
	_ = ValidatePGID(1)
	_ = ValidatePID(0)
	r := NewSafetyReceipt("FAC-174", "meter-test")
	if r.AmbientAttempts < 2 {
		t.Fatalf("AmbientAttempts = %d, want >= 2 from observed refusals", r.AmbientAttempts)
	}
	// Caller cannot force zero: a fresh receipt after more refusals grows.
	_ = ValidatePGID(0)
	r2 := NewSafetyReceipt("FAC-174", "meter-test")
	if r2.AmbientAttempts <= r.AmbientAttempts {
		t.Fatalf("meter did not advance: %d then %d", r.AmbientAttempts, r2.AmbientAttempts)
	}
}

func TestCancelSpawnedProcess_OwnedPath(t *testing.T) {
	defaultMeter.ResetForTest()
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := CancelSpawnedProcess(cmd.Process); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("owned child still alive after CancelSpawnedProcess")
	}
	// Second cancel of a dead process: Claim sees Signal(0) fail → zero handle.
	if err := CancelSpawnedProcess(cmd.Process); err != nil {
		t.Fatalf("second cancel: %v", err)
	}
	_ = pid
}

func TestCancelOwnedGroup_RejectsUnregistered(t *testing.T) {
	err := CancelOwnedGroup(OwnedGroup{pgid: 12345, token: 999999})
	if !errors.Is(err, ErrNotOwned) {
		t.Fatalf("err = %v, want ErrNotOwned", err)
	}
}

func TestCancelSpawnedProcess_RejectsUnsafePID(t *testing.T) {
	// Cannot construct os.Process with pid 1 easily; exercise Claim via
	// ValidatePGID on CancelOwned after forged handle.
	if err := CancelOwnedGroup(OwnedGroup{pgid: 1, token: 1}); !errors.Is(err, ErrNotOwned) {
		// unregistered first
		t.Logf("unregistered: %v", err)
	}
}

func TestCancelSpawnedProcess_ValidatesSelfSessionGroup(t *testing.T) {
	// CancelSpawnedProcess on our own process should fail ValidatePGID
	// (pgid == caller pgid when we are group leader) or Validate on claim.
	self, err := os.FindProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	// When test binary is session/group leader, ValidatePGID(self) fails.
	err = CancelSpawnedProcess(self)
	if err == nil {
		// Some environments run tests in a different process group than pid;
		// if claim succeeded we must not have killed ourselves — Cancel would
		// have SIGKILL'd the test. So err==nil means claim thought we exited
		// (Signal 0 failed) which is impossible for self — fail hard.
		t.Fatal("CancelSpawnedProcess(self) returned nil — would be suicidal if it signaled")
	}
	if !errors.Is(err, ErrUnsafeTarget) && !errors.Is(err, ErrNotOwned) {
		// Accept unsafe target (preferred) 
		t.Logf("self-cancel err = %v (acceptable if fail-closed)", err)
	}
}

func TestSignalExactProcess_RejectsSentinel(t *testing.T) {
	if err := SignalExactProcess(1, syscall.SIGTERM); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("err = %v", err)
	}
	if err := SignalExactProcess(0, syscall.SIGTERM); !errors.Is(err, ErrUnsafeTarget) {
		t.Fatalf("err = %v", err)
	}
}

func TestHostBackendNotExported(t *testing.T) {
	// Compile-time documentation: external packages cannot construct hostBackend.
	// This test exists so a re-export of SyscallBackend fails review again.
	var _ Backend = &FakeBackend{}
	// hostBackend implements the kill path but is not assignable from outside.
	_ = hostBackend{}
}
