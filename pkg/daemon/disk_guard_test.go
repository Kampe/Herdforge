package daemon

import (
	"context"
	"errors"
	"io"
	"sync"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/server"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// TestMain disables the disk-pressure floors so the package's existing
// engine/loop tests stay hermetic on a pressured host (FAC-153 incident
// host sat at 99%). Guard-assertion tests re-enable floors via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(preflight.EnvDiskMinFreeGB, "0")
	os.Setenv(preflight.EnvDiskMinFreePct, "0")
	os.Setenv(preflight.EnvDiskMinInodePct, "0")
	// Isolate the cross-process reservation ledger: tests must never read
	// from or release into the host's real ledger.
	dir, err := os.MkdirTemp("", "herd-disk-ledger-test-")
	if err != nil {
		panic(err)
	}
	os.Setenv(preflight.EnvDiskLedgerDir, dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestRunPulse_RefusedUnderDiskPressure(t *testing.T) {
	// 1 ZiB floor: any real volume reads as critically low.
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")

	tp := &timeoutProvider{failAfter: 1 << 30, tasks: []*provider.Task{
		{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
	}}
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, tp, nil, nil, nil, nil)

	task, err := e.RunPulse(context.Background(), "worker")
	// Pressure is an explicit BLOCKED error — never nil,nil ("no work") and
	// never a claimed task ("success").
	if err == nil || task != nil {
		t.Fatalf("expected refusal, got task=%v err=%v", task, err)
	}
	if !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected disk_pressure evidence, got: %v", err)
	}
	if tp.listOK.Load() != 0 || tp.claims.Load() != 0 {
		t.Fatalf("board consulted under pressure: list=%d claims=%d", tp.listOK.Load(), tp.claims.Load())
	}
	if e.DiskStatus() != "BLOCKED(disk_pressure)" {
		t.Fatalf("DiskStatus = %q", e.DiskStatus())
	}
}

func TestForgeLoop_NoDispatchUnderDiskPressure(t *testing.T) {
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")

	// Healthy provider with an urgent to-do task: only disk pressure can
	// explain a skipped dispatch.
	tp := &timeoutProvider{failAfter: 1 << 30, tasks: []*provider.Task{
		{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
	}}
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, tp, nil, nil, nil, nil)
	logs := []string{}
	d := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{0, 2}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs,
	}
	_ = e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 2})
	for _, a := range d.actions {
		if strings.HasPrefix(a, "dispatch:") {
			t.Fatalf("dispatched under disk pressure: %v", d.actions)
		}
	}
	blocked := false
	for _, l := range logs {
		if strings.Contains(l, "BLOCKED(disk_pressure)") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("skip was not surfaced as BLOCKED(disk_pressure): %v", logs)
	}
}

func TestForgeLoop_SerializesDispatchUnderSoftPressure(t *testing.T) {
	// Reserve floors stay zeroed (TestMain) so the hard gate passes on any
	// host; a saturated soft floor puts every real volume in the soft band.
	t.Setenv(preflight.EnvDiskSerializeFreeGB, "1099511627776")

	tp := &timeoutProvider{failAfter: 1 << 30, tasks: []*provider.Task{
		{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
	}}
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, tp, nil, nil, nil, nil)

	// Busy lanes: serialized fan-out must NOT dispatch more work.
	logs := []string{}
	busy := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 2, Max: 4}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs,
	}
	_ = e.ForgeLoop(context.Background(), busy, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 2})
	for _, a := range busy.actions {
		if strings.HasPrefix(a, "dispatch:") {
			t.Fatalf("dispatched with busy lanes under soft pressure: %v", busy.actions)
		}
	}
	serialized := false
	for _, l := range logs {
		if strings.Contains(l, "serializing dispatch") {
			serialized = true
		}
	}
	if !serialized {
		t.Fatalf("serialization not surfaced: %v", logs)
	}

	// Idle lanes: serialized fan-out still allows exactly the next dispatch —
	// soft pressure degrades parallelism, it does not stop work.
	logs2 := []string{}
	idle := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 0, Max: 4}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs2,
	}
	_ = e.ForgeLoop(context.Background(), idle, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1})
	dispatched := false
	for _, a := range idle.actions {
		if strings.HasPrefix(a, "dispatch:") {
			dispatched = true
		}
	}
	if !dispatched {
		t.Fatalf("soft pressure stopped work entirely (actions=%v logs=%v)", idle.actions, logs2)
	}
}

func TestForgeLoop_StartsProductionControlPlane(t *testing.T) {
	root := t.TempDir()
	t.Setenv(server.EnvControlToken, "test-capability")
	t.Setenv("HERD_ROOT", root) // canonical-root override for a non-git fixture
	tp := &timeoutProvider{failAfter: 1 << 30, tasks: []*provider.Task{}}
	wm := worktree.NewWorktreePool(root, t.TempDir())
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, tp, nil, nil, wm, nil)
	logs := []string{}
	d := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{0, 2}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs,
	}
	_ = e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1, ControlAddr: "127.0.0.1:0"})
	started := false
	for _, l := range logs {
		if strings.Contains(l, "control server on 127.0.0.1:") {
			started = true
		}
	}
	if !started {
		t.Fatalf("production control plane not started: %v", logs)
	}
}

func TestEngineStartControlPlaneServesLiveMetrics(t *testing.T) {
	root := t.TempDir()
	t.Setenv(server.EnvControlToken, "test-capability")
	t.Setenv("HERD_ROOT", root)
	wm := worktree.NewWorktreePool(root, t.TempDir())
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, &timeoutProvider{failAfter: 1 << 30}, nil, nil, wm, nil)

	cs, err := e.StartControlPlane(context.Background(), "127.0.0.1:0", func(msg string) { t.Log(msg) })
	if err != nil {
		t.Fatalf("StartControlPlane: %v", err)
	}
	defer func() { _ = cs.Stop(context.Background()) }()

	resp, err := http.Get("http://" + cs.BoundAddr() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "herd_disk_pressure_state") {
		t.Fatalf("live metrics missing (status %d):\n%s", resp.StatusCode, body)
	}
	if cs.ServeErr() != nil {
		t.Fatalf("serve error: %v", cs.ServeErr())
	}
}

func TestForgeLoop_AllActionsGatedByAdmission(t *testing.T) {
	// The renudge fixture: builder reported done but unverified. Under an
	// impossible floor the common admission gate must refuse the renudge
	// side effect too — not only dispatch.
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")
	e := forgeEngine(t,
		&provider.Task{ID: "1", Ref: "FAC-1", Status: "in-progress", Priority: provider.PriorityHigh},
	)
	logs := []string{}
	d := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 1, Max: 2}, completed: map[string]bool{"FAC-1": true}, verified: map[string]bool{}},
		logs:       &logs,
	}
	if err := e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1}); err != nil {
		t.Fatal(err)
	}
	if len(d.actions) != 0 {
		t.Fatalf("side effect ran under pressure: %v", d.actions)
	}
	skipped := false
	for _, l := range logs {
		if strings.Contains(l, "skip renudge (disk pressure)") {
			skipped = true
		}
	}
	if !skipped {
		t.Fatalf("renudge skip not surfaced: %v", logs)
	}
}

func TestForgeLoop_ConfiguredControlPlaneFailsClosed(t *testing.T) {
	// Occupy a port so the configured control plane cannot bind: an
	// explicitly configured control plane failing to start must abort the
	// loop, never silently run without it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	root := t.TempDir()
	t.Setenv(server.EnvControlToken, "test-capability")
	t.Setenv("HERD_ROOT", root)
	tp := &timeoutProvider{failAfter: 1 << 30, tasks: []*provider.Task{}}
	wm := worktree.NewWorktreePool(root, t.TempDir())
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, tp, nil, nil, wm, nil)
	logs := []string{}
	d := &recordingDriver{
		fakeDriver: fakeDriver{lanes: LaneState{0, 2}, completed: map[string]bool{}, verified: map[string]bool{}},
		logs:       &logs,
	}
	err = e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: time.Millisecond, MaxTicks: 1, ControlAddr: ln.Addr().String()})
	if err == nil || !strings.Contains(err.Error(), "failing closed") {
		t.Fatalf("configured control plane bind failure must abort the loop, got: %v (logs=%v)", err, logs)
	}
	if len(d.actions) != 0 {
		t.Fatalf("loop drove actions despite failed control plane: %v", d.actions)
	}
}

// lockedDriver is a goroutine-safe recording driver: the control plane's
// OnServeError callback logs from a different goroutine than the loop.
type lockedDriver struct {
	fakeDriver
	mu      sync.Mutex
	logLine []string
}

func (l *lockedDriver) Log(msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.logLine = append(l.logLine, msg)
}

func (l *lockedDriver) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.logLine...)
}

func TestForgeLoop_RuntimeControlPlaneDeathCancelsLoop(t *testing.T) {
	root := t.TempDir()
	t.Setenv(server.EnvControlToken, "test-capability")
	t.Setenv("HERD_ROOT", root)
	tp := &timeoutProvider{failAfter: 1 << 30, tasks: []*provider.Task{
		{ID: "1", Ref: "FAC-1", Status: "to-do", Priority: provider.PriorityUrgent},
	}}
	e := NewEngine(&config.Config{TaskProvider: config.TaskProvider{ProjectID: "p1"}}, tp, nil, nil, worktree.NewWorktreePool(root, t.TempDir()), nil)
	d := &lockedDriver{
		fakeDriver: fakeDriver{lanes: LaneState{Busy: 2, Max: 2}, completed: map[string]bool{}, verified: map[string]bool{}},
	}

	done := make(chan error, 1)
	go func() {
		done <- e.ForgeLoop(context.Background(), d, ForgeLoopOptions{Interval: 5 * time.Millisecond, MaxTicks: 1000, ControlAddr: "127.0.0.1:0"})
	}()
	// Wait for the control plane, then kill it at runtime.
	var cs *server.ControlServer
	for i := 0; i < 200; i++ {
		if cs = e.ControlPlane(); cs != nil && cs.BoundAddr() != "" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cs == nil {
		t.Fatal("control plane never started")
	}
	cs.RecordServeFailure(errors.New("listener died"))

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "control_plane_dead") {
			t.Fatalf("loop must cancel BLOCKED(control_plane_dead), got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("loop kept running after control plane death")
	}
	blocked := false
	for _, l := range d.snapshot() {
		if strings.Contains(l, "BLOCKED(control_plane_dead)") {
			blocked = true
		}
	}
	if !blocked {
		t.Fatalf("death not projected in logs: %v", d.snapshot())
	}
}
