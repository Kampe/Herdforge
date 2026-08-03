package preflight

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func fakeProber(stats map[string]DiskStat, errs map[string]error) DiskProber {
	return func(path string) (DiskStat, error) {
		if err, ok := errs[path]; ok {
			return DiskStat{}, err
		}
		st, ok := stats[path]
		if !ok {
			return DiskStat{}, fmt.Errorf("unexpected probe path %q", path)
		}
		return st, nil
	}
}

// healthyStat has ample headroom against the 15GiB/2%/1% defaults.
func healthyStat(path, fsid string) DiskStat {
	return DiskStat{
		Path: path, FSID: fsid,
		TotalBytes: 1000 << 30, FreeBytes: 500 << 30, FreePct: 50,
		TotalInodes: 1_000_000, FreeInodes: 900_000, InodeFreePct: 90,
	}
}

// incidentStat mirrors the 2026-08-02 live incident: 926GiB volume, 13GiB
// free, ~1.4% free. Must be refused under defaults.
func incidentStat(path, fsid string) DiskStat {
	return DiskStat{
		Path: path, FSID: fsid,
		TotalBytes: 926 << 30, FreeBytes: 13 << 30, FreePct: 1.4,
		TotalInodes: 156_000_000, FreeInodes: 17_000_000, InodeFreePct: 11,
	}
}

func asPressureErr(t *testing.T, err error) *DiskPressureError {
	t.Helper()
	if err == nil {
		t.Fatal("expected fail-closed error, got nil")
	}
	var pe *DiskPressureError
	if !errors.As(err, &pe) {
		t.Fatalf("expected *DiskPressureError, got %T: %v", err, err)
	}
	if pe.State != "BLOCKED" {
		t.Fatalf("expected state BLOCKED, got %q", pe.State)
	}
	return pe
}

func TestDiskGuardRefusesBelowThreshold(t *testing.T) {
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": incidentStat("/repo", "a")}, nil))
	pe := asPressureErr(t, g.Check("worktree_create", "/repo"))
	if pe.Reason != ReasonDiskPressure {
		t.Fatalf("reason = %q, want %q", pe.Reason, ReasonDiskPressure)
	}
	if pe.Operation != "worktree_create" {
		t.Fatalf("operation = %q", pe.Operation)
	}
	if !g.Blocked() {
		t.Fatal("guard should be in blocked state")
	}
	msg := pe.Error()
	for _, want := range []string{"BLOCKED", "disk_pressure", "worktree_create", "safe-GC", "evidence:"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error message missing %q: %s", want, msg)
		}
	}
	// Evidence carries observed values, thresholds, and fs identity.
	if len(pe.Stats) != 1 || pe.Stats[0].FSID != "a" || pe.Stats[0].FreeBytes != 13<<30 {
		t.Fatalf("evidence stats wrong: %+v", pe.Stats)
	}
	if pe.Thresholds.MinFreeBytes == 0 {
		t.Fatal("evidence thresholds missing")
	}
}

func TestDiskGuardAllowsAboveThreshold(t *testing.T) {
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": healthyStat("/repo", "a")}, nil))
	if err := g.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("healthy volume refused: %v", err)
	}
	if g.Blocked() {
		t.Fatal("guard should not be blocked")
	}
}

func TestDiskGuardConfigurableThresholds(t *testing.T) {
	// Raise the floor above a healthy volume: must refuse.
	t.Setenv(EnvDiskMinFreeGB, "600")
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": healthyStat("/repo", "a")}, nil))
	asPressureErr(t, g.Check("dispatch", "/repo"))

	// Zero disables every axis: even the incident volume passes.
	t.Setenv(EnvDiskMinFreeGB, "0")
	t.Setenv(EnvDiskMinFreePct, "0")
	t.Setenv(EnvDiskMinInodePct, "0")
	g2 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": incidentStat("/repo", "a")}, nil))
	if err := g2.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("disabled thresholds still refused: %v", err)
	}
}

func TestDiskGuardUnreadableStatFailsClosed(t *testing.T) {
	g := NewDiskGuard(fakeProber(nil, map[string]error{"/repo": errors.New("io error")}))
	pe := asPressureErr(t, g.Check("dispatch", "/repo"))
	if pe.Reason != ReasonStatUnreadable {
		t.Fatalf("reason = %q, want %q", pe.Reason, ReasonStatUnreadable)
	}
	if !g.Blocked() {
		t.Fatal("unreadable stat must leave guard blocked")
	}
}

func TestDiskGuardInodeExhaustionFailsClosed(t *testing.T) {
	st := healthyStat("/repo", "a")
	st.FreeInodes = 100
	st.InodeFreePct = 0.01
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	asPressureErr(t, g.Check("verifier_fanout", "/repo"))
}

func TestDiskGuardHysteresisRecovery(t *testing.T) {
	// Defaults: block below 15GiB, recover above 15*1.25 = 18.75GiB.
	current := incidentStat("/repo", "a") // 13GiB → block
	g := NewDiskGuard(func(string) (DiskStat, error) { return current, nil })
	asPressureErr(t, g.Check("dispatch", "/repo"))

	// Above block floor but below recover floor: still refused (recovering).
	current.FreeBytes = 16 << 30
	current.FreePct = 1.7
	t.Setenv(EnvDiskMinFreePct, "0") // isolate the bytes axis
	pe := asPressureErr(t, g.Check("dispatch", "/repo"))
	if pe.Reason != ReasonRecovering {
		t.Fatalf("reason = %q, want %q", pe.Reason, ReasonRecovering)
	}

	// Fresh probe above recover floor: allowed again.
	current.FreeBytes = 30 << 30
	current.FreePct = 3.2
	if err := g.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("recovered volume still refused: %v", err)
	}
	if g.Blocked() {
		t.Fatal("guard should have cleared blocked state")
	}
}

func TestDiskGuardWorstVolumeGoverns(t *testing.T) {
	// Repo volume healthy, temp volume diverges into pressure.
	g := NewDiskGuard(fakeProber(map[string]DiskStat{
		"/repo": healthyStat("/repo", "a"),
		"/tmpv": incidentStat("/tmpv", "b"),
	}, nil))
	pe := asPressureErr(t, g.Check("archive", "/repo", "/tmpv"))
	if pe.Reason != ReasonDiskPressure {
		t.Fatalf("reason = %q", pe.Reason)
	}
	// Same-FSID paths dedupe: only two distinct volumes in evidence.
	if len(pe.Stats) != 2 {
		t.Fatalf("expected 2 deduped volumes, got %d", len(pe.Stats))
	}
}

func TestRealDiskStatReadsThisVolume(t *testing.T) {
	st, err := realDiskStat(t.TempDir())
	if err != nil {
		t.Fatalf("realDiskStat: %v", err)
	}
	if st.TotalBytes == 0 || st.FSID == "" {
		t.Fatalf("implausible stat: %+v", st)
	}
	// Nonexistent leaf walks up to an existing ancestor instead of failing.
	st2, err := realDiskStat(t.TempDir() + "/not/yet/created")
	if err != nil {
		t.Fatalf("ancestor walk failed: %v", err)
	}
	if st2.FSID != st.FSID {
		t.Fatalf("ancestor walk landed on wrong volume: %q vs %q", st2.FSID, st.FSID)
	}
}
