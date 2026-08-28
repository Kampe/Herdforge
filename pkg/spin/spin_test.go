package spin

import (
	"testing"
	"time"
)

func TestEmptyContinuationLoopIsNonProductive(t *testing.T) {
	if !EmptyContinuationLoop(308, "\n") {
		t.Fatal("empty continuation loop must be detected")
	}
	if EmptyContinuationLoop(308, "completed work") {
		t.Fatal("productive output must not be empty")
	}
}

func TestSlowFirstTokenIsNotNonProductive(t *testing.T) {
	th := Thresholds{StallSamples: 2, SpinSamples: 2}
	_, findings := Classify(nil, Sample{AgentStatus: "working", Continuation: 1}, th, 0)
	for _, finding := range findings {
		if finding == NonProductive {
			t.Fatal("a first silent continuation must remain productive")
		}
	}
}

// Without noise-stripping a "thinking" pane redraws forever and STALL can
// never fire. This is the load-bearing behaviour of the whole detector.
func TestFrozenPaneFingerprintsIdenticallyDespiteLiveNoise(t *testing.T) {
	frame1 := "⠋\nThinking about the router split\n1234 tokens\ncontext 42%\n12s\nesc to interrupt"
	frame2 := "⠙\nThinking about the router split\n9876 tokens\ncontext 43%\n45s\nesc to interrupt"
	if Fingerprint(frame1) != Fingerprint(frame2) {
		t.Fatalf("a frozen pane must fingerprint identically:\n %q\n %q",
			NormalizeTail(frame1), NormalizeTail(frame2))
	}
	// Real progress must still change the fingerprint.
	frame3 := "⠹\nNow editing pkg/router/launch.go\n9876 tokens\n46s"
	if Fingerprint(frame1) == Fingerprint(frame3) {
		t.Fatal("genuinely new output must change the fingerprint")
	}
}

func TestNormalizeStripsAnsiAndCollapsesWhitespace(t *testing.T) {
	got := NormalizeTail("\x1b[32mhello\x1b[0m    world\n\n  again  ")
	if got != "hello world again" {
		t.Fatalf("normalize = %q", got)
	}
}

// Research and coordinator work produces no git delta by design; firing SPIN
// there would be constant noise.
func TestWriterClassGatesGitDeltaSpin(t *testing.T) {
	if !IsWriter("smith", "/repo/.herd/worktrees/fac-72") {
		t.Fatal("an agent in a worktree is writer-class")
	}
	if IsWriter("herdforge-orchestrator", "/repo/.herd/worktrees/x") {
		t.Fatal("the coordinator is never writer-class")
	}
	if IsWriter("scout-planner", "/repo/.herd/worktrees/x") {
		t.Fatal("planner work produces no git delta by design")
	}
	if IsWriter("smith", "/repo") {
		t.Fatal("the shared checkout is not writer-class")
	}
}

func mk(status, fp, head string, dirty int, writer bool) Sample {
	return Sample{PaneID: "p1", Name: "smith", AgentStatus: status,
		Fingerprint: fp, Head: head, Dirty: dirty, Writer: writer}
}

func TestStallFiresOnlyAfterEnoughFrozenSamples(t *testing.T) {
	th := DefaultThresholds() // StallSamples = 2
	s1, f := Classify(nil, mk("working", "aaa", "h1", 0, false), th, 0)
	if len(f) != 0 {
		t.Fatalf("first observation cannot stall: %v", f)
	}
	s2, f := Classify(&s1, mk("working", "aaa", "h1", 0, false), th, 0)
	if len(f) != 0 {
		t.Fatalf("one frozen sample is not yet a stall: %v", f)
	}
	_, f = Classify(&s2, mk("working", "aaa", "h1", 0, false), th, 0)
	if len(f) != 1 || f[0] != Stall {
		t.Fatalf("two frozen samples must fire STALL, got %v", f)
	}
}

// A slow-but-live agent must never accumulate toward a false STALL.
func TestAnyMovementResetsTheCounters(t *testing.T) {
	th := DefaultThresholds()
	s1, _ := Classify(nil, mk("working", "aaa", "h1", 0, true), th, 0)
	s2, _ := Classify(&s1, mk("working", "aaa", "h1", 0, true), th, 0)
	if s2.StallHits != 1 {
		t.Fatalf("stall hits = %d", s2.StallHits)
	}
	s3, f := Classify(&s2, mk("working", "bbb", "h1", 0, true), th, 0)
	if s3.StallHits != 0 || len(f) != 0 {
		t.Fatalf("new output must reset stall: hits=%d findings=%v", s3.StallHits, f)
	}
	// A dirty-count change resets SPIN even with identical output.
	s4, _ := Classify(&s3, mk("working", "bbb", "h1", 1, true), th, 0)
	if s4.SpinHits != 0 {
		t.Fatalf("dirty change must reset spin, got %d", s4.SpinHits)
	}
}

func TestSpinRequiresWriterClassAndEnoughSamples(t *testing.T) {
	th := Thresholds{StallSamples: 99, SpinSamples: 2}
	// Non-writer: no git delta, but SPIN must not fire.
	prev := &Sample{AgentStatus: "working", Head: "h1", Dirty: 0, SpinHits: 5}
	_, f := Classify(prev, mk("working", "aaa", "h1", 0, false), th, 0)
	for _, x := range f {
		if x == Spin {
			t.Fatal("SPIN must never fire for a non-writer agent")
		}
	}
	// Writer with a frozen git snapshot does fire.
	_, f = Classify(prev, mk("working", "aaa", "h1", 0, true), th, 0)
	found := false
	for _, x := range f {
		if x == Spin {
			found = true
		}
	}
	if !found {
		t.Fatalf("writer with frozen git snapshot must SPIN, got %v", f)
	}
}

// An idle or blocked pane is a different problem; reporting it here would
// drown the real signal.
func TestNonWorkingPanesProduceNoFindings(t *testing.T) {
	th := Thresholds{StallSamples: 1, SpinSamples: 1}
	prev := &Sample{AgentStatus: "working", Fingerprint: "aaa", StallHits: 9, SpinHits: 9}
	out, f := Classify(prev, mk("idle", "aaa", "h1", 0, true), th, time.Hour)
	if len(f) != 0 {
		t.Fatalf("idle pane must produce no findings, got %v", f)
	}
	if out.StallHits != 0 || out.SpinHits != 0 {
		t.Fatalf("counters must reset when a pane stops working: %+v", out)
	}
}

func TestLongIsPerClassAdvisory(t *testing.T) {
	if LongLimit(ClassReview) != DefaultLongReview {
		t.Fatal("review limit")
	}
	if LongLimit(ClassResearch) <= LongLimit(ClassBuild) {
		t.Fatal("research must be allowed to run longer than a build")
	}
	th := Thresholds{StallSamples: 99, SpinSamples: 99}
	_, f := Classify(nil, mk("working", "aaa", "h1", 0, false), th, 2*time.Hour)
	if len(f) != 1 || f[0] != Long {
		t.Fatalf("over-limit working time must advise LONG, got %v", f)
	}
}

func TestClassForBucketsByName(t *testing.T) {
	for name, want := range map[string]Class{
		"assayer":       ClassReview,
		"review-fac-72": ClassReview,
		"fix-fac-83":    ClassFix,
		"scout-planner": ClassResearch,
		"smith":         ClassBuild,
	} {
		if got := ClassFor(name); got != want {
			t.Errorf("ClassFor(%q) = %q, want %q", name, got, want)
		}
	}
}
