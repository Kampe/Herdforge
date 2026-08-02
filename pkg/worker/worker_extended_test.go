package worker

import (
	"context"
	"testing"
)

func TestShutdownLane_NotFound(t *testing.T) {
	sup := NewLaneSupervisor(".")
	err := sup.ShutdownLane("nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent lane")
	}
}

func TestSpinLane_StoresLane(t *testing.T) {
	sup := NewLaneSupervisor("/repo")
	lane, err := sup.SpinLane(context.Background(), "lane-42", "herd-smith", "claude", "/tmp/wt-42")
	if err != nil {
		t.Fatalf("expected clean spin, got err: %v", err)
	}
	if lane.LaneID != "lane-42" {
		t.Errorf("expected lane-42, got %s", lane.LaneID)
	}
	if _, exists := sup.Lanes["lane-42"]; !exists {
		t.Error("expected lane-42 to exist in supervisor")
	}

	// Shutdown should succeed now
	if err := sup.ShutdownLane("lane-42"); err != nil {
		t.Fatalf("expected clean shutdown, got: %v", err)
	}
	if _, exists := sup.Lanes["lane-42"]; exists {
		t.Error("expected lane-42 to be removed after shutdown")
	}
}
