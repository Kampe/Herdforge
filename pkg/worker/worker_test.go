package worker

import (
	"context"
	"testing"
)

func TestLaneSupervisor_SpinAndShutdown(t *testing.T) {
	sup := NewLaneSupervisor(".")

	lane, err := sup.SpinLane(context.Background(), "lane-1", "herd-smith", "claude", "/tmp/wt-1")
	if err != nil || lane == nil {
		t.Fatalf("expected clean lane spin, got err: %v", err)
	}

	if lane.LaneID != "lane-1" || lane.Role != "herd-smith" {
		t.Errorf("unexpected lane fields: %+v", lane)
	}

	if err := sup.ShutdownLane("lane-1"); err != nil {
		t.Errorf("expected clean lane shutdown, got err: %v", err)
	}
}
