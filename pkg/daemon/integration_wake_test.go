package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestForgeIntegrationWakeExecutesAndReportsFailure(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	calls := 0
	opts := fastLoop(1)
	opts.IntegrationWakes = func(context.Context) error { calls++; return errors.New("owner delivery unavailable") }
	err := e.ForgeLoop(context.Background(), d, opts)
	if calls != 1 || err == nil || !strings.Contains(err.Error(), "owner delivery unavailable") {
		t.Fatalf("wake failure hidden: calls=%d err=%v", calls, err)
	}
	if len(d.actions) != 0 {
		t.Fatal("wake automatically performed a board action", d.actions)
	}
}

func TestForgeIntegrationWakeRecoveryClearsFailure(t *testing.T) {
	e := forgeEngine(t)
	d := &fakeDriver{lanes: LaneState{Max: 1}}
	calls := 0
	opts := fastLoop(2)
	opts.IntegrationWakes = func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("temporary")
		}
		return nil
	}
	if err := e.ForgeLoop(context.Background(), d, opts); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expected each tick to drive wake, got %d", calls)
	}
}
