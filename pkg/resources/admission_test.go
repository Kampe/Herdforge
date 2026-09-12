package resources

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/freshness"
)

var admissionNow = time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)

func testLimits() Limits {
	return Limits{CPURefuseLoad: 1.0, MemReservePct: 15, StaleAfter: 30 * time.Second, ProbeTimeout: time.Second}
}

func freshCPU(normalized float64) freshness.Reading[CPULoad] {
	return freshness.Fresh("test", admissionNow, CPULoad{Load1: normalized * 8, CPUs: 8, Normalized: normalized})
}

func freshMem(freePct int) freshness.Reading[MemHeadroom] {
	// Linux-shaped: MemAvailable means what a percentage implies, so it gates.
	return freshness.Fresh("test", admissionNow, MemHeadroom{FreePct: freePct, FreePctGates: true})
}

func unknownCPU() freshness.Reading[CPULoad] {
	return unknownReading[CPULoad]("test", errors.New("probe exploded"), "fix the probe")
}

func unknownMem() freshness.Reading[MemHeadroom] {
	return unknownReading[MemHeadroom]("test", errors.New("probe exploded"), "fix the probe")
}

func reasonsMentioning(a Admission, needle string) bool {
	for _, r := range a.Reasons {
		if strings.Contains(strings.ToLower(r), strings.ToLower(needle)) {
			return true
		}
	}
	return false
}

// An absence of measurement is not headroom. This is the whole card in one
// test: every shape of "we could not tell" must refuse, and none of them may
// render as a healthy percentage.
func TestUnknownObservationsRefuse(t *testing.T) {
	cases := []struct {
		name string
		cpu  freshness.Reading[CPULoad]
		mem  freshness.Reading[MemHeadroom]
		want string
	}{
		{"cpu unknown", unknownCPU(), freshMem(90), "cpu"},
		{"memory unknown", freshCPU(0.1), unknownMem(), "memory"},
		{"both unknown", unknownCPU(), unknownMem(), "cpu"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Decide(admissionNow, c.cpu, c.mem, testLimits())
			if a.Admits() {
				t.Fatalf("unknown observation admitted: %s", a.Explain())
			}
			if !reasonsMentioning(a, c.want) {
				t.Fatalf("refusal did not name %s: %v", c.want, a.Reasons)
			}
			if AdmissionVerdict(a) != VerdictAlert {
				t.Fatalf("verdict = %q, want %q for an unmeasured host", AdmissionVerdict(a), VerdictAlert)
			}
			if GatePasses(AdmissionVerdict(a)) {
				t.Fatal("an unmeasured host cleared the gate")
			}
		})
	}
}

// A held answer that has aged out is history, not a current reading. Refusing
// on history is safe; admitting on it is what this card exists to stop.
func TestStaleObservationsRefuse(t *testing.T) {
	limits := testLimits()
	staleCPU := freshness.Degrade(freshCPU(0.1), "test", errors.New("probe timed out"), "retry")
	staleMem := freshness.Degrade(freshMem(90), "test", errors.New("probe timed out"), "retry")
	later := admissionNow.Add(limits.StaleAfter + time.Second)

	if a := Decide(later, staleCPU, freshMem(90), limits); a.Admits() {
		t.Fatalf("stale cpu admitted: %s", a.Explain())
	}
	if a := Decide(later, freshCPU(0.1), staleMem, limits); a.Admits() {
		t.Fatalf("stale memory admitted: %s", a.Explain())
	}
	// Within the window a held answer is still usable, so the limit is a real
	// boundary and not a blanket refusal of everything stale.
	if a := Decide(admissionNow.Add(time.Second), staleCPU, staleMem, limits); !a.Admits() {
		t.Fatalf("a freshly degraded but in-window reading refused: %s", a.Explain())
	}
}

// CPU and memory must refuse INDEPENDENTLY. An operator who fixes only the
// constraint that happened to be checked first would otherwise retry straight
// into the other one.
func TestCPUAndMemoryRefuseIndependently(t *testing.T) {
	limits := testLimits()

	cpuOnly := Decide(admissionNow, freshCPU(1.5), freshMem(90), limits)
	if cpuOnly.Admits() {
		t.Fatal("saturated cpu with healthy memory admitted")
	}
	if len(cpuOnly.Reasons) != 1 || !reasonsMentioning(cpuOnly, "cpu is saturated") {
		t.Fatalf("want exactly one cpu reason, got %v", cpuOnly.Reasons)
	}

	memOnly := Decide(admissionNow, freshCPU(0.1), freshMem(5), limits)
	if memOnly.Admits() {
		t.Fatal("memory below the reserve with idle cpu admitted")
	}
	if len(memOnly.Reasons) != 1 || !reasonsMentioning(memOnly, "os reserve") {
		t.Fatalf("want exactly one memory reason, got %v", memOnly.Reasons)
	}

	both := Decide(admissionNow, freshCPU(1.5), freshMem(5), limits)
	if len(both.Reasons) != 2 {
		t.Fatalf("both constraints breached must report both reasons, got %v", both.Reasons)
	}
	// A measured refusal is TIGHT, not ALERT: we could tell, and the answer was no.
	if AdmissionVerdict(both) != VerdictTight {
		t.Fatalf("verdict = %q, want %q for a measured refusal", AdmissionVerdict(both), VerdictTight)
	}
}

// The OS reserve is explicit and its boundary is pinned: headroom equal to the
// reserve is still admitted, below it is not.
func TestHealthyAdmitsAtTheReserveBoundary(t *testing.T) {
	limits := testLimits()
	if a := Decide(admissionNow, freshCPU(0.5), freshMem(limits.MemReservePct), limits); !a.Admits() {
		t.Fatalf("headroom exactly at the reserve refused: %s", a.Explain())
	}
	if a := Decide(admissionNow, freshCPU(0.5), freshMem(limits.MemReservePct-1), limits); a.Admits() {
		t.Fatal("headroom one point below the reserve admitted")
	}
	// The load limit is likewise a boundary, not a range.
	if a := Decide(admissionNow, freshCPU(limits.CPURefuseLoad), freshMem(90), limits); a.Admits() {
		t.Fatal("normalized load exactly at the limit admitted")
	}
	healthy := Decide(admissionNow, freshCPU(0.5), freshMem(90), limits)
	if !healthy.Admits() || AdmissionVerdict(healthy) != VerdictOK || !GatePasses(AdmissionVerdict(healthy)) {
		t.Fatalf("a healthy known host did not admit: %s", healthy.Explain())
	}
	if len(healthy.Reasons) != 0 {
		t.Fatalf("an admission carried refusal reasons: %v", healthy.Reasons)
	}
}

// An impossible reading is not a reading. 101% free is a parser bug, and a
// parser bug must not be able to claim the most headroom possible.
func TestImpossibleReadingsRefuse(t *testing.T) {
	for _, pct := range []int{-5, 101, 1000} {
		a := Decide(admissionNow, freshCPU(0.1), freshMem(pct), testLimits())
		if a.Admits() {
			t.Fatalf("free_pct=%d admitted", pct)
		}
	}
	zeroCPUs := freshness.Fresh("test", admissionNow, CPULoad{Load1: 0, CPUs: 0})
	if a := Decide(admissionNow, zeroCPUs, freshMem(90), testLimits()); a.Admits() {
		t.Fatal("a zero cpu count admitted; load cannot be normalized by an assumed width")
	}
}

// FAC-693 must not be re-broken: swap residue is a scar, not a wound. A host
// with a large sticky swap reading and real headroom still admits.
func TestStickySwapAloneNeverRefuses(t *testing.T) {
	mem := freshness.Fresh("test", admissionNow, MemHeadroom{Pressure: PressureNormal, FreePct: 80, FreePctGates: true, SwapMB: 30720, SwapKnown: true})
	a := Decide(admissionNow, freshCPU(0.2), mem, testLimits())
	if !a.Admits() {
		t.Fatalf("30GB of historical swap refused a healthy host: %s", a.Explain())
	}
}

// The reporting shape must never render an unknown as a number.
func TestSnapshotFromUnknownReportsMinusOne(t *testing.T) {
	clearEnv(t)
	s := SnapshotFrom(Decide(admissionNow, unknownCPU(), unknownMem(), testLimits()))
	if s.FreePct != -1 {
		t.Fatalf("FreePct = %d, want -1", s.FreePct)
	}
	if s.Verdict != VerdictAlert || GatePasses(s.Verdict) {
		t.Fatalf("verdict %q cleared the gate", s.Verdict)
	}
	if s.Admission == nil || len(s.Admission.Reasons) == 0 {
		t.Fatal("the snapshot dropped the reasons the refusal was made for")
	}
}

// Explain is what an operator reads. It must never be blank, and a refusal must
// carry its numbers.
func TestExplainIsAlwaysActionable(t *testing.T) {
	refused := Decide(admissionNow, freshCPU(2), freshMem(1), testLimits())
	if !strings.Contains(refused.Explain(), "refusing heavy work") {
		t.Fatalf("refusal explanation was not actionable: %q", refused.Explain())
	}
	admitted := Decide(admissionNow, freshCPU(0.1), freshMem(90), testLimits())
	if !strings.Contains(admitted.Explain(), "admitted") {
		t.Fatalf("admission explanation was blank or wrong: %q", admitted.Explain())
	}
}

// One parser for both platforms' load formats. A malformed reading is an error,
// never zero -- zero load is the healthiest possible answer and is exactly what
// a broken probe must not be able to assert.
func TestParseLoad1(t *testing.T) {
	ok := map[string]float64{
		"{ 1.25 1.40 1.55 }":        1.25,
		"1.25 1.40 1.55 2/345 6789": 1.25,
		"  0.00 0.01 0.05 1/2 3":    0.00,
	}
	for in, want := range ok {
		got, err := parseLoad1(in)
		if err != nil || got != want {
			t.Errorf("parseLoad1(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "{ }", "notanumber 1 2", "{ -3.0 1 2 }"} {
		if _, err := parseLoad1(bad); err == nil {
			t.Errorf("parseLoad1(%q) returned no error; a malformed load must not read as idle", bad)
		}
	}
}

// The strict counterpart to parseMemoryPressureFreePct: every malformed shape
// that the legacy parser answers with 100 is an error here.
func TestParseFreePctStrict(t *testing.T) {
	const label = "memory free percentage:"
	got, err := parseFreePctStrict("System-wide memory free percentage: 67%\n", label)
	if err != nil || got != 67 {
		t.Fatalf("parseFreePctStrict = %d, %v; want 67, nil", got, err)
	}
	for _, bad := range []string{
		"",
		"nothing relevant here\n",
		"System-wide memory free percentage: \n",
		"System-wide memory free percentage: none%\n",
		"System-wide memory free percentage: 140%\n",
	} {
		if v, err := parseFreePctStrict(bad, label); err == nil {
			t.Errorf("parseFreePctStrict(%q) = %d with no error; the legacy 100 fallback is the defect", bad, v)
		}
	}
	// The legacy parser is retained for the informational path, and it still
	// answers 100 to the same input. Pinned deliberately so the difference
	// between the reporting parser and the deciding parser stays visible.
	if legacy := parseMemoryPressureFreePct("nothing relevant here\n"); legacy != 100 {
		t.Fatalf("legacy parser = %d, want its documented 100; the split is intentional", legacy)
	}
}

// darwinMem is the Darwin shape: the kernel pressure level decides and the free
// percentage is along for the report only.
func darwinMem(level PressureLevel, freePct int) freshness.Reading[MemHeadroom] {
	return freshness.Fresh("test", admissionNow, MemHeadroom{Pressure: level, FreePct: freePct, FreePctGates: false})
}

// TestDarwinLowFreePercentIsNotDanger is the followup-3022 regression, and it
// is taken from a real measurement rather than an invented one.
//
// The performance guard observed this host at 2026-09-12T18:26:31Z: 1993MiB
// unused, 10GiB held by the compressor, swap 0, and
// kern.memorystatus_vm_pressure_level reading 1. That is roughly 4% free and
// entirely healthy. The first version of this package gated on the free
// percentage and would have refused it -- the FAC-693 mistake in new clothes,
// committed in the same file whose comments claimed to be avoiding it.
func TestDarwinLowFreePercentIsNotDanger(t *testing.T) {
	guardObserved := darwinMem(PressureNormal, 4)
	a := Decide(admissionNow, freshCPU(0.256), guardObserved, testLimits())
	if !a.Admits() {
		t.Fatalf("a healthy Mac at 4%% free was refused: %s", a.Explain())
	}
	if strings.Contains(a.Explain(), "reserve") {
		t.Fatalf("the reserve was applied to a non-gating free percentage: %s", a.Explain())
	}
}

// The kernel's own warning refuses, no matter how much memory looks free.
func TestKernelPressureRefusesRegardlessOfFreePercent(t *testing.T) {
	for _, level := range []PressureLevel{PressureWarn, PressureCritical} {
		t.Run(level.String(), func(t *testing.T) {
			a := Decide(admissionNow, freshCPU(0.1), darwinMem(level, 95), testLimits())
			if a.Admits() {
				t.Fatalf("kernel pressure %s admitted at 95%% free: %s", level, a.Explain())
			}
			if !reasonsMentioning(a, "kernel reports memory pressure") {
				t.Fatalf("refusal did not cite the kernel signal: %v", a.Reasons)
			}
		})
	}
}

// A reading with neither an authoritative pressure level nor a gating headroom
// figure is not an observation, and must not be treated as one.
func TestMemoryWithNoDecidingSignalRefuses(t *testing.T) {
	empty := freshness.Fresh("test", admissionNow, MemHeadroom{Pressure: PressureUnknown, FreePct: 99, FreePctGates: false})
	a := Decide(admissionNow, freshCPU(0.1), empty, testLimits())
	if a.Admits() {
		t.Fatalf("a reading with no deciding signal admitted at 99%% free: %s", a.Explain())
	}
	if !reasonsMentioning(a, "no signal allowed to decide") {
		t.Fatalf("refusal did not explain the missing signal: %v", a.Reasons)
	}
}

// The kernel level is a fixed vocabulary. Anything else -- including silence --
// is unknown, never "normal".
func TestParseDarwinPressureLevel(t *testing.T) {
	ok := map[string]PressureLevel{"1": PressureNormal, "2\n": PressureWarn, " 4 ": PressureCritical}
	for in, want := range ok {
		got, err := parseDarwinPressureLevel(in)
		if err != nil || got != want {
			t.Errorf("parseDarwinPressureLevel(%q) = %v, %v; want %v, nil", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "   ", "normal", "0", "3", "-1", "7"} {
		got, err := parseDarwinPressureLevel(bad)
		if err == nil {
			t.Errorf("parseDarwinPressureLevel(%q) = %v with no error; silence must not read as normal", bad, got)
		}
		if got != PressureUnknown {
			t.Errorf("parseDarwinPressureLevel(%q) = %v, want PressureUnknown on error", bad, got)
		}
	}
	if PressureUnknown.Known() || PressureUnknown.Unsafe() {
		t.Fatal("PressureUnknown must be neither known nor unsafe")
	}
	if !PressureNormal.Known() || PressureNormal.Unsafe() {
		t.Fatal("PressureNormal must be known and safe")
	}
}

// strconv.ParseFloat accepts "NaN", "Inf" and "+Inf". NaN is false in EVERY
// comparison, so a NaN load or a NaN threshold turns `normalized >= limit` into
// a test no host can fail -- the gate admits unconditionally while looking like
// it is still checking. An infinite threshold does the same thing.
func TestNonFiniteNumbersNeverDisableTheThreshold(t *testing.T) {
	for _, bad := range []string{"NaN", "nan", "Inf", "+Inf", "-Inf", "inf"} {
		t.Run("load_"+bad, func(t *testing.T) {
			if v, err := parseLoad1(bad + " 1 2"); err == nil {
				t.Fatalf("parseLoad1(%q) = %v with no error; a value no threshold can exceed is not a measurement", bad, v)
			}
		})
		t.Run("override_"+bad, func(t *testing.T) {
			t.Setenv("HERD_CPU_REFUSE_LOAD", bad)
			if got := DefaultLimits().CPURefuseLoad; got != defaultCPURefuseLoad {
				t.Fatalf("HERD_CPU_REFUSE_LOAD=%q produced limit %v; an invalid override must fall back to %v, never disable the limit",
					bad, got, defaultCPURefuseLoad)
			}
		})
	}
	// A non-finite limit reaching Decide directly (it is a public function) is
	// repaired and REPORTED, and the repaired limit still judges the load.
	for _, limit := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), 0, -1} {
		l := testLimits()
		l.CPURefuseLoad = limit
		a := Decide(admissionNow, freshCPU(4.0), freshMem(90), l)
		if a.Admits() {
			t.Fatalf("limit %v admitted a normalized load of 4.0: %s", limit, a.Explain())
		}
		if !reasonsMentioning(a, "configuration repaired") {
			t.Fatalf("limit %v was repaired silently: %v", limit, a.Reasons)
		}
	}
	// And a healthy host is still admitted after the repair: the repair is not
	// itself a refusal.
	l := testLimits()
	l.CPURefuseLoad = math.NaN()
	if a := Decide(admissionNow, freshCPU(0.1), freshMem(90), l); !a.Admits() {
		t.Fatalf("a repaired limit refused a healthy host: %s", a.Explain())
	}
}

// Normalized is a plain struct field on a public type, so it is caller-supplied
// data and not a fact. A reading whose Normalized disagrees with its own Load1
// and CPUs is rejected rather than reconciled: there is no way to know which
// field was the lie.
func TestNormalizedIsDerivedNotTrusted(t *testing.T) {
	forged := freshness.Fresh("test", admissionNow, CPULoad{Load1: 32, CPUs: 8, Normalized: 0.01})
	a := Decide(admissionNow, forged, freshMem(90), testLimits())
	if a.Admits() {
		t.Fatalf("a forged normalized value admitted a load1 of 32 over 8 cpus: %s", a.Explain())
	}
	if !reasonsMentioning(a, "inconsistent") {
		t.Fatalf("refusal did not name the inconsistency: %v", a.Reasons)
	}

	for _, bad := range []CPULoad{
		{Load1: math.NaN(), CPUs: 8, Normalized: math.NaN()},
		{Load1: math.Inf(1), CPUs: 8, Normalized: math.Inf(1)},
		{Load1: -1, CPUs: 8, Normalized: -0.125},
		{Load1: 1, CPUs: 0, Normalized: 0},
		{Load1: 1, CPUs: -4, Normalized: 0},
		{Load1: 8, CPUs: 8, Normalized: math.NaN()},
	} {
		r := freshness.Fresh("test", admissionNow, bad)
		if a := Decide(admissionNow, r, freshMem(90), testLimits()); a.Admits() {
			t.Errorf("unusable cpu reading %+v admitted: %s", bad, a.Explain())
		}
	}
}

// pkg/freshness deliberately answers "usable" for a ZERO Reading and never
// calls a FRESH reading stale however old it is. Those are reasonable defaults
// for its other consumers, so this policy layer tightens them locally rather
// than changing a shared API other callers depend on.
func TestConsumerPolicyEnforcesWhatFreshnessDoesNot(t *testing.T) {
	limits := testLimits()

	// A zero Reading: State "", no timestamp. freshness.Value() reports ok=true
	// for it, which would hand Decide a zero CPULoad that looks perfectly idle.
	var zeroCPU freshness.Reading[CPULoad]
	if _, ok := zeroCPU.Value(); !ok {
		t.Fatal("precondition changed: freshness now rejects a zero Reading, so this guard may be reconsidered")
	}
	if a := Decide(admissionNow, zeroCPU, freshMem(90), limits); a.Admits() {
		t.Fatalf("a zero-valued reading admitted: %s", a.Explain())
	}

	// FRESH but ancient. StaleBeyond returns false for FRESH at any age.
	ancient := freshness.Fresh("test", admissionNow.Add(-time.Hour), CPULoad{Load1: 0.8, CPUs: 8, Normalized: 0.1})
	if ancient.StaleBeyond(admissionNow, limits.StaleAfter) {
		t.Fatal("precondition changed: freshness now ages out FRESH readings itself")
	}
	a := Decide(admissionNow, ancient, freshMem(90), limits)
	if a.Admits() {
		t.Fatalf("an hour-old FRESH reading admitted: %s", a.Explain())
	}
	if !reasonsMentioning(a, "beyond the") {
		t.Fatalf("refusal did not cite the age window: %v", a.Reasons)
	}

	// A timestamp from the future is not a very fresh reading.
	future := freshness.Fresh("test", admissionNow.Add(time.Hour), CPULoad{Load1: 0.8, CPUs: 8, Normalized: 0.1})
	if a := Decide(admissionNow, future, freshMem(90), limits); a.Admits() {
		t.Fatalf("a reading timestamped an hour ahead admitted: %s", a.Explain())
	}

	// A non-positive StaleAfter must not switch the age check off.
	for _, window := range []time.Duration{0, -time.Minute} {
		l := testLimits()
		l.StaleAfter = window
		if a := Decide(admissionNow, ancient, freshMem(90), l); a.Admits() {
			t.Fatalf("StaleAfter=%s disabled the age check: %s", window, a.Explain())
		}
	}

	// An unrecognized state is not a posture.
	bogus := freshness.Fresh("test", admissionNow, CPULoad{Load1: 0.8, CPUs: 8, Normalized: 0.1})
	bogus.State = freshness.State("PROBABLY_FINE")
	if a := Decide(admissionNow, bogus, freshMem(90), limits); a.Admits() {
		t.Fatalf("an unrecognized freshness state admitted: %s", a.Explain())
	}
}

// The old parser scanned backwards for trailing digits, so it silently rewrote
// "-50%" into 50 and "1.50%" into 50 -- turning a negative or fractional
// reading into a healthy whole number.
func TestStrictPercentRejectsRewrittenTokens(t *testing.T) {
	const label = "memory free percentage:"
	for _, line := range []string{
		"System-wide memory free percentage: -50%",
		"System-wide memory free percentage: 1.50%",
		"System-wide memory free percentage: +50%",
		"System-wide memory free percentage: 5e1%",
		"System-wide memory free percentage: 50,0%",
		"System-wide memory free percentage: 0x32%",
		"System-wide memory free percentage: 50 extra%",
	} {
		if got, err := parseFreePctStrict(line+"\n", label); err == nil {
			t.Errorf("parseFreePctStrict(%q) = %d with no error; the value was rewritten into a healthy number", line, got)
		}
	}
	if got, err := parseFreePctStrict("System-wide memory free percentage: 67%\n", label); err != nil || got != 67 {
		t.Fatalf("a plain whole percentage stopped parsing: %d, %v", got, err)
	}
}

// The JSON a consumer actually receives must carry the NUMBERS the safety
// decision used. freshness.Reading keeps its value private and defines no
// MarshalJSON, so encoding the readings directly emits posture and prose and
// silently drops every observation.
func TestPublicJSONCarriesTheObservedNumbers(t *testing.T) {
	clearEnv(t)
	healthy := Decide(admissionNow, freshCPU(0.25), darwinMem(PressureNormal, 4), testLimits())
	decoded := decodeSnapshot(t, SnapshotFrom(healthy, admissionNow))

	admission, ok := decoded["admission"].(map[string]any)
	if !ok {
		t.Fatalf("no admission object in %v", decoded)
	}
	cpu := admission["cpu"].(map[string]any)
	if cpu["known"] != true {
		t.Fatalf("cpu reported unknown for a healthy reading: %v", cpu)
	}
	if cpu["load1"] == nil || cpu["cpus"] == nil || cpu["normalized"] == nil {
		t.Fatalf("cpu numbers were dropped from the JSON: %v", cpu)
	}
	if got := cpu["normalized"].(float64); math.Abs(got-0.25) > 1e-9 {
		t.Fatalf("normalized = %v, want the derived 0.25", got)
	}
	mem := admission["memory"].(map[string]any)
	if mem["pressure"] != "normal" || mem["pressure_known"] != true {
		t.Fatalf("the pressure level used for the decision is missing: %v", mem)
	}
	if mem["free_pct_gates"] != false {
		t.Fatalf("the JSON must say whether the percentage was allowed to decide: %v", mem)
	}
	if admission["explanation"] == "" || admission["verdict"] != VerdictOK {
		t.Fatalf("admission summary missing: %v", admission)
	}
}

// An UNKNOWN reading must OMIT its numbers, never publish a zero. A zero load
// and a zero percentage are both plausible healthy-looking values.
func TestPublicJSONOmitsUnknownsRatherThanZeroingThem(t *testing.T) {
	clearEnv(t)
	refused := Decide(admissionNow, unknownCPU(), unknownMem(), testLimits())
	decoded := decodeSnapshot(t, SnapshotFrom(refused, admissionNow))

	if decoded["verdict"] != VerdictAlert {
		t.Fatalf("verdict = %v, want ALERT", decoded["verdict"])
	}
	if decoded["free_pct"].(float64) != -1 {
		t.Fatalf("free_pct = %v, want -1 for an unmeasured host", decoded["free_pct"])
	}
	admission := decoded["admission"].(map[string]any)
	cpu := admission["cpu"].(map[string]any)
	for _, field := range []string{"load1", "cpus", "normalized"} {
		if _, present := cpu[field]; present {
			t.Errorf("unknown cpu published %q = %v; an absence must not render as a number", field, cpu[field])
		}
	}
	if cpu["known"] != false || cpu["state"] != string(freshness.StateUnknown) {
		t.Fatalf("unknown cpu did not report itself unknown: %v", cpu)
	}
	mem := admission["memory"].(map[string]any)
	if _, present := mem["free_pct"]; present {
		t.Errorf("unknown memory published free_pct = %v", mem["free_pct"])
	}
	if mem["pressure"] != "unknown" || mem["pressure_known"] != false {
		t.Fatalf("unknown memory did not report unknown pressure: %v", mem)
	}
	if reasons, _ := admission["reasons"].([]any); len(reasons) == 0 {
		t.Fatal("a refusal published no reasons")
	}
}

func decodeSnapshot(t *testing.T, s Snapshot) map[string]any {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode snapshot json: %v", err)
	}
	return decoded
}

// Timing is explicit end to end: Decide, Report and every pure helper use the
// clock they are GIVEN. Before this, SnapshotFrom called Report(time.Now()) and
// NormalizedCPU/AdmissionVerdict each consulted the wall clock again, so a
// fixed-clock decision rendered at the real current time silently lost its
// observations -- and worse, a held decision could render admits=true with
// known=false and no numbers behind it.
func TestReportRendersAtTheClockItIsGiven(t *testing.T) {
	healthy := Decide(admissionNow, freshCPU(0.25), freshMem(80), testLimits())
	if !healthy.Admits() {
		t.Fatalf("precondition: %s", healthy.Explain())
	}

	// Rendered at the decision instant: everything is present.
	fresh := healthy.Report(admissionNow)
	if !fresh.Admits || fresh.Verdict != VerdictOK {
		t.Fatalf("a fresh decision did not render as admitted: %+v", fresh)
	}
	if !fresh.CPU.Known || fresh.CPU.Normalized == nil || !fresh.Memory.Known {
		t.Fatalf("a fresh decision dropped its observations: cpu=%+v mem=%+v", fresh.CPU, fresh.Memory)
	}
	if fresh.DecidedAt == "" || fresh.RenderedAt == "" {
		t.Fatalf("report did not state its own timing: %+v", fresh)
	}

	// The SAME decision, rendered after its observations aged out, must not
	// still claim ADMIT. This is the combination root caught: admits=true with
	// known=false and nothing behind it.
	expired := healthy.Report(admissionNow.Add(testLimits().StaleAfter + time.Second))
	if expired.Admits || expired.Verdict == VerdictOK {
		t.Fatalf("an expired decision still admitted: %+v", expired)
	}
	if expired.CPU.Known || expired.CPU.Normalized != nil {
		t.Fatalf("an expired decision published cpu numbers: %+v", expired.CPU)
	}
	if expired.Memory.Known || expired.Memory.FreePct != nil {
		t.Fatalf("an expired decision published memory numbers: %+v", expired.Memory)
	}
	if len(expired.Reasons) == 0 || !strings.Contains(expired.Explanation, "no longer current") {
		t.Fatalf("an expired decision did not say why: %+v", expired)
	}

	// Revalidation never turns a refusal into an admission.
	refused := Decide(admissionNow, freshCPU(4), freshMem(80), testLimits())
	if r := refused.Report(admissionNow); r.Admits {
		t.Fatalf("a refusal rendered as admitted: %+v", r)
	}
}

// The pure helpers must not consult a clock of their own, or a fixed-time
// fixture and the decision it made would disagree.
func TestPureHelpersUseNoHiddenClock(t *testing.T) {
	// admissionNow is far in the past relative to any real run. If these
	// consulted time.Now() they would report the reading as aged out.
	a := Decide(admissionNow, freshCPU(0.25), freshMem(80), testLimits())
	if normalized, ok := a.NormalizedCPU(); !ok || math.Abs(normalized-0.25) > 1e-9 {
		t.Fatalf("NormalizedCPU = %v, %v; want the value resolved at DecidedAt", normalized, ok)
	}
	if _, ok := a.Headroom(); !ok {
		t.Fatal("Headroom lost the reading resolved at DecidedAt")
	}
	if AdmissionVerdict(a) != VerdictOK {
		t.Fatalf("AdmissionVerdict = %q; a pure helper consulted a different clock", AdmissionVerdict(a))
	}
	if !strings.Contains(a.Explain(), "normalized cpu load 0.25") {
		t.Fatalf("Explain lost its numbers: %q", a.Explain())
	}
	if !a.DecidedAt.Equal(admissionNow) {
		t.Fatalf("DecidedAt = %v, want the supplied clock", a.DecidedAt)
	}
}
