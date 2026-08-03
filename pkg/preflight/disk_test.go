package preflight

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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
	// Defaults: block below 15GiB reserve + 2GiB headroom = 17GiB,
	// recover above (15+2)*1.25 = 21.25GiB.
	current := incidentStat("/repo", "a") // 13GiB → block
	g := NewDiskGuard(func(string) (DiskStat, error) { return current, nil })
	asPressureErr(t, g.Check("dispatch", "/repo"))

	// Above block floor but below recover floor: still refused (recovering).
	current.FreeBytes = 18 << 30
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

func TestDiskGuardStateProjection(t *testing.T) {
	current := incidentStat("/repo", "a")
	g := NewDiskGuard(func(string) (DiskStat, error) { return current, nil })

	// Fresh guard (as after restart): no stale state, projects ok.
	if g.State() != DiskOK || g.Status() != "ok" || g.LastEvidence() != nil {
		t.Fatalf("fresh guard: state=%s status=%s", g.State(), g.Status())
	}

	// Pressure → BLOCKED(disk_pressure), evidence exposed for projection.
	_ = g.Check("dispatch", "/repo")
	if g.State() != DiskBlocked || g.Status() != "BLOCKED(disk_pressure)" {
		t.Fatalf("blocked: state=%s status=%s", g.State(), g.Status())
	}
	if ev := g.LastEvidence(); ev == nil || ev.Reason != ReasonDiskPressure {
		t.Fatalf("evidence not exposed: %+v", g.LastEvidence())
	}

	// Hysteresis window → recovering label, still refusing (block floor is
	// 17GiB with default headroom; recover floor 21.25GiB).
	t.Setenv(EnvDiskMinFreePct, "0")
	current.FreeBytes = 18 << 30
	if err := g.Check("dispatch", "/repo"); err == nil {
		t.Fatal("recovering window must still refuse")
	}
	if g.State() != DiskRecovering || g.Status() != "recovering" || !g.Blocked() {
		t.Fatalf("recovering: state=%s status=%s blocked=%v", g.State(), g.Status(), g.Blocked())
	}

	// Stable headroom → ok, evidence cleared. Pressure never projected as
	// success while refusing; ok only after an allowing probe.
	current.FreeBytes = 30 << 30
	if err := g.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("recovered: %v", err)
	}
	if g.State() != DiskOK || g.Status() != "ok" || g.LastEvidence() != nil {
		t.Fatalf("ok: state=%s status=%s", g.State(), g.Status())
	}
}

func TestDiskGuardUnreadableStatusLabel(t *testing.T) {
	g := NewDiskGuard(fakeProber(nil, map[string]error{"/repo": errors.New("io error")}))
	_ = g.Check("dispatch", "/repo")
	if g.Status() != "BLOCKED(disk_stat_unreadable)" {
		t.Fatalf("status = %q", g.Status())
	}
}

func TestDiskGuardRestartReconciliation(t *testing.T) {
	// Old process blocked; a restart must NOT erase hysteresis. A fresh
	// guard whose first probe lands inside the recovery band (above block,
	// below recover floor) reconstructs recovering and keeps refusing.
	t.Setenv(EnvDiskMinFreeGB, "15")
	old := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": incidentStat("/repo", "a")}, nil))
	_ = old.Check("dispatch", "/repo")
	if !old.Blocked() {
		t.Fatal("old process should be blocked")
	}

	st := healthyStat("/repo", "a")
	st.FreeBytes = 18 << 30 // above block floor (17GiB incl. headroom), below recover floor (21.25GiB)
	st.FreePct = 50
	fresh := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	err := fresh.Check("dispatch", "/repo")
	if err == nil {
		t.Fatal("fresh guard in the recovery band must refuse, not resume")
	}
	pe := asPressureErr(t, err)
	if pe.Reason != ReasonRecovering || fresh.State() != DiskRecovering {
		t.Fatalf("expected reconstructed recovering, got reason=%s state=%s", pe.Reason, fresh.State())
	}

	// Above the recover floor, a fresh process reconciles straight to ok.
	st2 := healthyStat("/repo", "a")
	fresh2 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st2}, nil))
	if err := fresh2.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("fresh guard above recover floor must be ok: %v", err)
	}
	if fresh2.State() != DiskOK {
		t.Fatalf("fresh guard state = %s", fresh2.State())
	}
}

func TestDiskGuardBuildHeadroomIncludedInDecision(t *testing.T) {
	// 16GiB free clears a 10GiB reserve + 2GiB default headroom (and the
	// 15GiB fresh-process recover floor), but not 10GiB + 8GiB explicit
	// required temp/build headroom.
	t.Setenv(EnvDiskMinFreeGB, "10")
	t.Setenv(EnvDiskMinFreePct, "0")
	st := incidentStat("/repo", "a")
	st.FreeBytes = 16 << 30
	st.FreePct = 50 // isolate the bytes axis

	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	if err := g.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("16GiB > 10+2GiB effective floor must pass: %v", err)
	}

	t.Setenv(EnvDiskBuildHeadroomGB, "8")
	g2 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	pe := asPressureErr(t, g2.Check("dispatch", "/repo"))
	if pe.Thresholds.HeadroomBytes != 8<<30 {
		t.Fatalf("headroom missing from evidence: %+v", pe.Thresholds)
	}
	// Recover floor scales from the effective floor: (10+8)*1.25 = 22.5GiB.
	if got := pe.Thresholds.RecoverFreeBytes; got != uint64(22.5*float64(1<<30)) {
		t.Fatalf("recover floor = %d, want 22.5GiB", got)
	}
}

func TestDiskGuardAdviseGraduatedShedding(t *testing.T) {
	t.Setenv(EnvDiskMinFreeGB, "15")
	t.Setenv(EnvDiskMinFreePct, "0")
	st := healthyStat("/repo", "a")
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))

	// Ample headroom (500GiB >> 2x15GiB soft floor): full parallelism.
	adv := g.Advise("verifier_fanout", "/repo")
	if adv.Verdict != AdviceProceed || adv.MaxParallel != 0 {
		t.Fatalf("proceed expected: %+v", adv)
	}

	// Soft band: 25GiB is above the 21.25GiB recover floor (block floor is
	// 17GiB with default headroom) but below the 34GiB default soft floor —
	// serialize before refusing any work.
	st.FreeBytes = 25 << 30
	g2 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	adv = g2.Advise("verifier_fanout", "/repo")
	if adv.Verdict != AdviceSerialize || adv.MaxParallel != 1 {
		t.Fatalf("serialize expected: %+v", adv)
	}
	if g2.Blocked() {
		t.Fatal("serialize band must not mark the guard blocked")
	}

	// Below the block floor: refuse with the same structured evidence.
	st.FreeBytes = 10 << 30
	g3 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	adv = g3.Advise("verifier_fanout", "/repo")
	if adv.Verdict != AdviceRefuse || adv.Evidence == nil || adv.Evidence.Reason != ReasonDiskPressure {
		t.Fatalf("refuse expected: %+v", adv)
	}
	if !g3.Blocked() {
		t.Fatal("refuse must drive the same state machine as Check")
	}

	// Soft floor is configurable: shrink it below 25GiB and the same
	// volume proceeds at full parallelism.
	t.Setenv(EnvDiskSerializeFreeGB, "22")
	st.FreeBytes = 25 << 30
	g4 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	if adv = g4.Advise("verifier_fanout", "/repo"); adv.Verdict != AdviceProceed {
		t.Fatalf("configured soft floor ignored: %+v", adv)
	}
}

func TestDiskGuardAdviseUnreadableRefuses(t *testing.T) {
	// Unknown capacity is never permission for even one mutation: an
	// unreadable probe inside Advise refuses and blocks the guard, exactly
	// like Check — never a softened "serialize".
	g := NewDiskGuard(fakeProber(nil, map[string]error{"/repo": errors.New("io error")}))
	adv := g.Advise("verifier_fanout", "/repo")
	if adv.Verdict != AdviceRefuse {
		t.Fatalf("unreadable probe must refuse, got: %+v", adv)
	}
	if adv.Evidence == nil || adv.Evidence.Reason != ReasonStatUnreadable {
		t.Fatalf("missing unreadable evidence: %+v", adv)
	}
	if !g.Blocked() || g.Status() != "BLOCKED(disk_stat_unreadable)" {
		t.Fatalf("guard not blocked after unreadable Advise: %s", g.Status())
	}
}

func TestDiskGuardDefaultHeadroomActive(t *testing.T) {
	// No env at all: the 2GiB build headroom is on by default, so 16GiB
	// free fails the 15+2GiB effective floor. The gate is not inert
	// without operator setup.
	st := healthyStat("/repo", "a")
	st.FreeBytes = 16 << 30
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	pe := asPressureErr(t, g.Check("worktree_create", "/repo"))
	if pe.Thresholds.HeadroomBytes != 2<<30 {
		t.Fatalf("default headroom missing from evidence: %+v", pe.Thresholds)
	}

	// Explicit headroom is honored even with the reserve floors zeroed.
	t.Setenv(EnvDiskMinFreeGB, "0")
	t.Setenv(EnvDiskMinFreePct, "0")
	t.Setenv(EnvDiskMinInodePct, "0")
	t.Setenv(EnvDiskBuildHeadroomGB, "20")
	g2 := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))
	asPressureErr(t, g2.Check("worktree_create", "/repo"))
}

func TestDiskGuardAbsurdFloorSaturatesFailClosed(t *testing.T) {
	// A 1 ZiB floor (used by wiring tests) exceeds uint64 byte range.
	// Conversion must saturate — an overflow that wraps to a tiny floor
	// would fail OPEN. Refusal must be plain disk_pressure, not a
	// wrap-corrupted recovering verdict.
	t.Setenv(EnvDiskMinFreeGB, "1099511627776")
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": healthyStat("/repo", "a")}, nil))
	pe := asPressureErr(t, g.Check("dispatch", "/repo"))
	if pe.Reason != ReasonDiskPressure {
		t.Fatalf("reason = %q, want %q", pe.Reason, ReasonDiskPressure)
	}
	if pe.Thresholds.MinFreeBytes != maxDiskFloorBytes {
		t.Fatalf("floor not saturated: %d", pe.Thresholds.MinFreeBytes)
	}
}

func TestDiskGuardNonFiniteThresholdsFailClosed(t *testing.T) {
	// NaN parses fine and NaN < x is always false: without rejection, a
	// NaN floor silently disables the gate (fails OPEN). All non-finite
	// values must fall back to the protective defaults, keeping the
	// incident volume refused — mutation-sensitive proof, not just parse.
	for _, hostile := range []string{"NaN", "nan", "+Inf", "-Inf", "Infinity"} {
		t.Run(hostile, func(t *testing.T) {
			t.Setenv(EnvDiskMinFreeGB, hostile)
			t.Setenv(EnvDiskMinFreePct, hostile)
			t.Setenv(EnvDiskMinInodePct, hostile)
			t.Setenv(EnvDiskRecoverFreeGB, hostile)
			t.Setenv(EnvDiskBuildHeadroomGB, hostile)
			g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": incidentStat("/repo", "a")}, nil))
			pe := asPressureErr(t, g.Check("dispatch", "/repo"))
			if pe.Thresholds.MinFreeBytes != gbToBytes(defaultDiskMinFreeGB) {
				t.Fatalf("floor not restored to default: %+v", pe.Thresholds)
			}
			if math.IsNaN(pe.Thresholds.MinFreePct) || math.IsInf(pe.Thresholds.MinFreePct, 0) {
				t.Fatalf("non-finite pct floor leaked: %+v", pe.Thresholds)
			}
		})
	}
}

func TestDiskGuardAdmitReservationsBoundConcurrentFanOut(t *testing.T) {
	// 20GiB free above a 10GiB floor with a 4GiB per-mutation reservation:
	// only 2 concurrent admissions fit ((20-10-4)/4 + 1); the rest refuse
	// against reservation-adjusted free space, not N stale snapshots.
	t.Setenv(EnvDiskMinFreeGB, "10")
	t.Setenv(EnvDiskMinFreePct, "0")
	t.Setenv(EnvDiskMinInodePct, "0")
	t.Setenv(EnvDiskBuildHeadroomGB, "4")
	t.Setenv(EnvDiskRecoverFreeGB, "0.001") // effectively no hysteresis band

	st := healthyStat("/repo", "a")
	st.FreeBytes = 20 << 30
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": st}, nil))

	var admitted int32
	var releases []func()
	var relMu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if rel, err := g.Admit("worktree_create", "/repo"); err == nil {
				atomic.AddInt32(&admitted, 1)
				relMu.Lock()
				releases = append(releases, rel)
				relMu.Unlock()
			}
		}()
	}
	wg.Wait()
	// Floor 10+4=14; admissions subtract 4GiB each: #1 sees 20 (ok, out=4),
	// #2 sees 16 (ok, out=8), #3 sees 12 < 14 (refused).
	if admitted != 2 {
		t.Fatalf("admitted %d, want exactly 2 bounded by reservations", admitted)
	}
	if pe := g.LastEvidence(); pe == nil || pe.OutstandingReservedBytes != 8<<30 {
		t.Fatalf("refusal evidence missing outstanding reservations: %+v", pe)
	}

	// Release restores capacity: idempotent, and a new admission fits.
	for _, rel := range releases {
		rel()
		rel() // double-release must not underflow accounting
	}
	if rel, err := g.Admit("worktree_create", "/repo"); err != nil {
		t.Fatalf("admission after release refused: %v", err)
	} else {
		rel()
	}
}

func TestDiskGuardProbeAndTransitionSerialized(t *testing.T) {
	// The prober itself asserts mutual exclusion: any overlap between
	// probe+transition sections is a serialization failure.
	var inFlight int32
	g := NewDiskGuard(func(path string) (DiskStat, error) {
		if atomic.AddInt32(&inFlight, 1) != 1 {
			t.Error("concurrent probe+transition detected")
		}
		defer atomic.AddInt32(&inFlight, -1)
		return healthyStat(path, "a"), nil
	})
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = g.Check("dispatch", "/repo")
			_ = g.Advise("dispatch", "/repo")
			if rel, err := g.Admit("dispatch", "/repo"); err == nil {
				rel()
			}
		}()
	}
	wg.Wait()
}

type recordingSink struct {
	mu     sync.Mutex
	states []DiskGuardState
	evs    []*DiskPressureError
}

func (r *recordingSink) RecordDiskState(s DiskGuardState, ev *DiskPressureError) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.states = append(r.states, s)
	r.evs = append(r.evs, ev)
}

func TestDiskGuardEvidenceSinkTransitionsOnly(t *testing.T) {
	current := incidentStat("/repo", "a")
	g := NewDiskGuard(func(string) (DiskStat, error) { return current, nil })
	sink := &recordingSink{}
	g.SetEvidenceSink(sink)

	_ = g.Check("dispatch", "/repo") // "" -> blocked
	_ = g.Check("dispatch", "/repo") // blocked -> blocked: NO event
	t.Setenv(EnvDiskMinFreePct, "0")
	current.FreeBytes = 18 << 30 // blocked -> recovering
	_ = g.Check("dispatch", "/repo")
	_ = g.Check("dispatch", "/repo") // recovering steady: NO event
	current.FreeBytes = 30 << 30
	current.FreePct = 3.2 // recovering -> ok
	_ = g.Check("dispatch", "/repo")

	sink.mu.Lock()
	defer sink.mu.Unlock()
	want := []DiskGuardState{DiskBlocked, DiskRecovering, DiskOK}
	if len(sink.states) != 3 || sink.states[0] != want[0] || sink.states[1] != want[1] || sink.states[2] != want[2] {
		t.Fatalf("transitions = %v, want %v", sink.states, want)
	}
	// BLOCKED and recovering carry structured evidence; ok clears it.
	if sink.evs[0] == nil || sink.evs[0].Reason != ReasonDiskPressure {
		t.Fatalf("blocked evidence: %+v", sink.evs[0])
	}
	if sink.evs[1] == nil || sink.evs[1].Reason != ReasonRecovering {
		t.Fatalf("recovering evidence: %+v", sink.evs[1])
	}
	if sink.evs[2] != nil {
		t.Fatalf("ok must carry nil evidence, got %+v", sink.evs[2])
	}
}

func TestDiskGuardEmptyScopeFailsClosed(t *testing.T) {
	g := NewDiskGuard(fakeProber(map[string]DiskStat{}, nil))
	for _, paths := range [][]string{nil, {}, {""}} {
		err := g.Check("dispatch", paths...)
		pe := asPressureErr(t, err)
		if pe.Reason != ReasonScopeUnknown {
			t.Fatalf("reason = %q, want %q", pe.Reason, ReasonScopeUnknown)
		}
	}
	if _, err := g.Admit("dispatch"); err == nil {
		t.Fatal("empty-scope Admit must refuse")
	}
	if adv := g.Advise("dispatch"); adv.Verdict != AdviceRefuse {
		t.Fatalf("empty-scope Advise must refuse: %+v", adv)
	}
}

func TestDiskGuardSoftFloorSaturates(t *testing.T) {
	// Both reserve and headroom saturated: 2x the block floor must clamp,
	// not wrap to a tiny soft floor.
	t.Setenv(EnvDiskMinFreeGB, "1099511627776")
	t.Setenv(EnvDiskBuildHeadroomGB, "1099511627776")
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": healthyStat("/repo", "a")}, nil))
	adv := g.Advise("dispatch", "/repo")
	if adv.Verdict != AdviceRefuse {
		t.Fatalf("saturated floors must refuse, got %+v", adv)
	}
}

func TestDiskEvidenceRedactsHostPaths(t *testing.T) {
	g := NewDiskGuard(fakeProber(map[string]DiskStat{
		"/Users/someone/secret-project/repo": incidentStat("/Users/someone/secret-project/repo", "abc123"),
	}, nil))
	pe := asPressureErr(t, g.Check("dispatch", "/Users/someone/secret-project/repo"))

	raw, err := json.Marshal(pe)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-project") || strings.Contains(string(raw), "/Users/") {
		t.Fatalf("host path leaked into persisted evidence: %s", raw)
	}
	if !strings.Contains(string(raw), `"volume":"vol-abc123"`) {
		t.Fatalf("bounded volume label missing: %s", raw)
	}
	// The full rendered error (what could reach Kaneo) is clean too.
	if strings.Contains(pe.Error(), "secret-project") {
		t.Fatalf("host path leaked into error string: %s", pe.Error())
	}
	// Path stays available process-local on the struct.
	if pe.Stats[0].Path == "" {
		t.Fatal("process-local path lost from struct")
	}

	// Unreadable-probe evidence redacts the path inside the os error.
	g2 := NewDiskGuard(func(path string) (DiskStat, error) {
		return DiskStat{}, &os.PathError{Op: "statfs", Path: path, Err: errors.New("io error")}
	})
	pe2 := asPressureErr(t, g2.Check("dispatch", "/Users/someone/secret-project/repo"))
	if strings.Contains(pe2.Error(), "secret-project") {
		t.Fatalf("unreadable evidence leaked path: %s", pe2.Error())
	}
}

func TestDiskGuardPerVolumeHysteresisSubsetCannotClear(t *testing.T) {
	// Temp volume (fsid b) blocks; a later Check probing only the healthy
	// repo volume (fsid a) must NOT clear the block — ready requires a
	// fresh positive probe of the volume that actually failed.
	repoSt := healthyStat("/repo", "a")
	tempSt := incidentStat("/tmpv", "b")
	g := NewDiskGuard(fakeProber(map[string]DiskStat{"/repo": repoSt, "/tmpv": tempSt}, nil))
	asPressureErr(t, g.Check("archive", "/repo", "/tmpv"))
	if g.State() != DiskBlocked {
		t.Fatalf("state = %s", g.State())
	}

	// Healthy-subset probe: still refused, evidence names the blocked vol.
	pe := asPressureErr(t, g.Check("dispatch", "/repo"))
	if !strings.Contains(pe.Detail, "vol-b") || !strings.Contains(pe.Detail, "fresh positive probe") {
		t.Fatalf("evidence must name the still-blocked volume: %s", pe.Detail)
	}
	if g.State() != DiskBlocked {
		t.Fatal("healthy subset cleared another volume's block")
	}

	// Temp volume recovers above the recover floor: full-scope probe clears.
	recovered := healthyStat("/tmpv", "b")
	g2prober := fakeProber(map[string]DiskStat{"/repo": repoSt, "/tmpv": recovered}, nil)
	// Swap prober by reusing the same guard through a fresh probe map.
	g.prober = g2prober
	if err := g.Check("dispatch", "/repo", "/tmpv"); err != nil {
		t.Fatalf("full-scope recovery refused: %v", err)
	}
	if g.State() != DiskOK {
		t.Fatalf("state = %s after full recovery", g.State())
	}
}

func TestDiskGuardUnknownScopeClearsOnlyOnFullRecovery(t *testing.T) {
	// An unreadable probe blocks via the synthetic unknown volume; a later
	// fully-positive probe pass (all volumes above recover) clears it.
	failing := true
	st := healthyStat("/repo", "a")
	g := NewDiskGuard(func(path string) (DiskStat, error) {
		if failing {
			return DiskStat{}, errors.New("io error")
		}
		return st, nil
	})
	asPressureErr(t, g.Check("dispatch", "/repo"))
	failing = false
	if err := g.Check("dispatch", "/repo"); err != nil {
		t.Fatalf("recovered probe refused: %v", err)
	}
	if g.State() != DiskOK {
		t.Fatalf("state = %s", g.State())
	}
}
