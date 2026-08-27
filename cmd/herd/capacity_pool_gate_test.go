package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-584: the capacity document existed, but review --pool never consulted it
// before preparing a candidate surface. These pins prove the gate refuses
// before mutation and that concurrent racers cannot all pass the same census.
func TestAcquirePoolCapacityRefusesWhenHerdrIsDown(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(dir, "admission.lease"))
	t.Cleanup(func() { poolCapacityObserve = observeCapacity })

	poolCapacityObserve = func() CapacityObservation {
		o := healthy()
		o.HerdrRunning = false
		o.MemTotalMiB = 48000
		return o
	}
	release, err := acquirePoolCapacityOrRefuse()
	if err == nil {
		if release != nil {
			release()
		}
		t.Fatal("pool gate admitted a host with herdr down")
	}
	if !strings.Contains(err.Error(), "REFUSING before candidate preparation") {
		t.Fatalf("refusal must name the pre-prep boundary, got %v", err)
	}
	if !strings.Contains(err.Error(), "herdr") {
		t.Fatalf("refusal must name herdr, got %v", err)
	}
}

func TestAcquirePoolCapacityRefusesWhenCensusIsUnreadable(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_ADMISSION_LEASE_PATH", filepath.Join(dir, "admission.lease"))
	t.Cleanup(func() { poolCapacityObserve = observeCapacity })

	poolCapacityObserve = func() CapacityObservation {
		o := healthy()
		o.AgentsListed = false
		o.MemTotalMiB = 48000
		return o
	}
	_, err := acquirePoolCapacityOrRefuse()
	if err == nil {
		t.Fatal("pool gate admitted an unreadable census")
	}
}

func TestAcquirePoolCapacityHoldsLeaseAgainstARacer(t *testing.T) {
	dir := t.TempDir()
	lease := filepath.Join(dir, "admission.lease")
	t.Setenv("HERD_ADMISSION_LEASE_PATH", lease)
	t.Setenv("HERD_ADMISSION_LEASE_TTL", "2m")
	t.Cleanup(func() { poolCapacityObserve = observeCapacity })

	poolCapacityObserve = func() CapacityObservation {
		o := healthy()
		o.MemTotalMiB = 48000
		return o
	}
	release, err := acquirePoolCapacityOrRefuse()
	if err != nil {
		t.Fatalf("first admit: %v", err)
	}
	defer release()

	_, err = acquirePoolCapacityOrRefuse()
	if err == nil {
		t.Fatal("second concurrent admit passed the same census")
	}
	if !strings.Contains(err.Error(), "admission lease") {
		t.Fatalf("racer must hit the lease, got %v", err)
	}
	if _, statErr := os.Stat(lease); statErr != nil {
		t.Fatalf("lease file missing while held: %v", statErr)
	}
}

func TestCapacityJSONContractCarriesSchemaAndSlots(t *testing.T) {
	c := decideCapacity(healthy(), 4, 512, 2048)
	if c.SchemaVersion != capacitySchemaVersion {
		t.Fatalf("schema_version=%d", c.SchemaVersion)
	}
	if c.ObservedAt == "" {
		t.Fatal("observed_at missing")
	}
	if _, err := time.Parse(time.RFC3339Nano, c.ObservedAt); err != nil {
		t.Fatalf("observed_at not RFC3339Nano: %v", err)
	}
	if c.AvailableSlots != 4 {
		t.Fatalf("available_slots=%d want 4 on an empty healthy host", c.AvailableSlots)
	}
}
