package daemon

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestMain disables the disk-pressure floors so the package's existing
// engine/loop tests stay hermetic on a pressured host (FAC-153 incident
// host sat at 99%). Guard-assertion tests re-enable floors via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(preflight.EnvDiskMinFreeGB, "0")
	os.Setenv(preflight.EnvDiskMinFreePct, "0")
	os.Setenv(preflight.EnvDiskMinInodePct, "0")
	os.Exit(m.Run())
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
