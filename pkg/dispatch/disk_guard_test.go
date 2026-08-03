package dispatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestMain disables the disk-pressure floors so the package's existing
// dispatch tests stay hermetic on a pressured host (the FAC-153 incident
// host sat at 99%). Guard-assertion tests re-enable floors via t.Setenv.
func TestMain(m *testing.M) {
	os.Setenv(preflight.EnvDiskMinFreeGB, "0")
	os.Setenv(preflight.EnvDiskMinFreePct, "0")
	os.Setenv(preflight.EnvDiskMinInodePct, "0")
	os.Exit(m.Run())
}

// countingTaskProvider records whether the board was consulted at all.
type countingTaskProvider struct {
	mockTaskProvider
	listCalls int
}

func (c *countingTaskProvider) ListTasks(ctx context.Context, projectID, status string) ([]*provider.Task, error) {
	c.listCalls++
	return c.mockTaskProvider.ListTasks(ctx, projectID, status)
}

func TestDispatchRefusesUnderDiskPressureBeforeAnyMutation(t *testing.T) {
	// 1 ZiB floor: any real volume reads as critically low.
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")

	tp := &countingTaskProvider{mockTaskProvider: mockTaskProvider{
		tasks: []*provider.Task{{ID: "1", Ref: "FAC-1", Title: "t", Status: "to-do"}},
	}}
	mw := &mockWorktree{err: fmt.Errorf("must never be reached")}
	d := &Dispatcher{
		Config: &config.Config{
			Project:      config.ProjectConfig{Name: "t", DefaultBranch: "main"},
			TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
			Lanes:        []config.LaneDef{{Name: "worker", Role: "worker"}},
		},
		TaskProvider: tp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
	}

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
	if err == nil {
		t.Fatal("expected fail-closed refusal under disk pressure")
	}
	if !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected structured disk_pressure evidence, got: %v", err)
	}
	// Refused BEFORE any external effect: no board list/claim, no worktree.
	if tp.listCalls != 0 {
		t.Fatalf("board consulted %d times despite pressure", tp.listCalls)
	}
	if mw.calls != 0 {
		t.Fatalf("worktree service called %d times despite pressure", mw.calls)
	}
}

func TestDispatchAllowedWithFloorsDisabled(t *testing.T) {
	// Floors zeroed by TestMain: the guard must not refuse; dispatch then
	// proceeds far enough to consult the board and worktree service.
	tp := &countingTaskProvider{mockTaskProvider: mockTaskProvider{
		tasks: []*provider.Task{{ID: "1", Ref: "FAC-1", Title: "t", Status: "to-do"}},
	}}
	mw := &mockWorktree{err: fmt.Errorf("stop here: guard already passed")}
	d := &Dispatcher{
		Config: &config.Config{
			Project:      config.ProjectConfig{Name: "t", DefaultBranch: "main"},
			TaskProvider: config.TaskProvider{Type: "memory", ProjectID: "test"},
			Lanes:        []config.LaneDef{{Name: "worker", Role: "worker"}},
		},
		TaskProvider: tp,
		Worktree:     mw,
		Compensator:  &recordingCompensator{},
		Herdr:        &fakeHerdr{available: false},
	}

	_, err := d.Dispatch(context.Background(), DispatchOptions{TicketRef: "FAC-1", NoLaunch: true})
	if err == nil || strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("guard interfered with healthy dispatch: %v", err)
	}
	if tp.listCalls == 0 || mw.calls == 0 {
		t.Fatalf("dispatch did not proceed past the guard (list=%d wt=%d)", tp.listCalls, mw.calls)
	}
}
