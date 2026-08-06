package quotasup

import (
	"testing"

	"github.com/Kampe/Herdforge/pkg/usage"
)

// A lane billed against the wrong pool reads as exhausted while its own pool
// is idle, and the supervisor reroutes work that never needed to move.
func TestQuotaPoolMapsIndependentlyMeteredPools(t *testing.T) {
	cases := []struct{ provider, model, want string }{
		{"codex", "gpt-5.3-codex-spark", "spark"},
		{"codex", "gpt-5.6-luna", "default"},
		{"claude", "claude-fable-5", "fable"},
		{"claude", "claude-sonnet-5", "default"},
		{"agy", "gemini-3-pro", "gemini"},
		{"agy", "claude-opus-4-6-thinking", "nonGemini"},
		{"grok", "grok-4.5", "default"},
		{"CODEX", "GPT-5.3-CODEX-SPARK", "spark"},
	}
	for _, c := range cases {
		if got := QuotaPool(c.provider, c.model); got != c.want {
			t.Errorf("QuotaPool(%q,%q) = %q, want %q", c.provider, c.model, got, c.want)
		}
	}
}

func TestQuotaProviderAliasesAgy(t *testing.T) {
	if got := QuotaProvider("agy"); got != "antigravity" {
		t.Fatalf("agy must map to the antigravity ledger, got %q", got)
	}
	if got := QuotaProvider("Claude"); got != "claude" {
		t.Fatalf("provider should normalise, got %q", got)
	}
}

func ptrB(b bool) *bool { return &b }
func ptrI(i int) *int   { return &i }

// An unreadable pool must never be treated as available.
func TestClassifyFailsClosedOnUnreadableQuota(t *testing.T) {
	if got := Classify(nil, DefaultWarnRunwayMinutes); got != Untracked {
		t.Fatalf("absent ledger row = %q, want untracked", got)
	}
	if got := Classify(&usage.BurnState{Stale: true}, DefaultWarnRunwayMinutes); got != Unknown {
		t.Fatalf("stale ledger = %q, want unknown", got)
	}
	for _, reason := range []string{"stale", "provider-error", "no-quota-data"} {
		if got := Classify(&usage.BurnState{Reason: reason}, DefaultWarnRunwayMinutes); got != Unknown {
			t.Fatalf("reason %q = %q, want unknown", reason, got)
		}
	}
}

func TestClassifyExhaustedAndAtRisk(t *testing.T) {
	if got := Classify(&usage.BurnState{Reason: "exhausted"}, DefaultWarnRunwayMinutes); got != Exhausted {
		t.Fatalf("exhausted reason = %q", got)
	}
	if got := Classify(&usage.BurnState{Class: usage.BurnExhausted}, DefaultWarnRunwayMinutes); got != Exhausted {
		t.Fatalf("exhausted class = %q", got)
	}
	atRisk := &usage.BurnState{ExhaustsBeforeReset: ptrB(true), RunwayMinutes: ptrI(30)}
	if got := Classify(atRisk, DefaultWarnRunwayMinutes); got != AtRisk {
		t.Fatalf("30m runway inside a 120m warning = %q, want at_risk", got)
	}
	// Projected to exhaust, but far enough out to be ordinary burn.
	far := &usage.BurnState{ExhaustsBeforeReset: ptrB(true), RunwayMinutes: ptrI(400)}
	if got := Classify(far, DefaultWarnRunwayMinutes); got != Healthy {
		t.Fatalf("400m runway = %q, want healthy", got)
	}
	// Burning, but not projected to exhaust before the window resets.
	fine := &usage.BurnState{ExhaustsBeforeReset: ptrB(false), RunwayMinutes: ptrI(10)}
	if got := Classify(fine, DefaultWarnRunwayMinutes); got != Healthy {
		t.Fatalf("not exhausting before reset = %q, want healthy", got)
	}
}

// A fresh supervisor run must not page the coordinator about a healthy fleet.
func TestFirstHealthyObservationIsBaselineNotAnIncident(t *testing.T) {
	if IsTransition(FirstObservation, Healthy) {
		t.Fatal("first healthy observation is a baseline, not a transition")
	}
	if !IsTransition(FirstObservation, Exhausted) {
		t.Fatal("first observation of an exhausted pool must be reported")
	}
	if IsTransition(Healthy, Healthy) {
		t.Fatal("no change is not a transition")
	}
	if !IsTransition(Exhausted, Healthy) {
		t.Fatal("recovery must be reported")
	}
	if !IsTransition(Healthy, AtRisk) {
		t.Fatal("degradation must be reported")
	}
}

func TestPriorFallsBackToFirstObservation(t *testing.T) {
	prev := &Snapshot{Agents: []Assignment{{Name: "smith", Capacity: AtRisk}}}
	if got := Prior(prev, "smith"); got != AtRisk {
		t.Fatalf("known lane = %q", got)
	}
	if got := Prior(prev, "scout"); got != FirstObservation {
		t.Fatalf("unseen lane = %q, want %q", got, FirstObservation)
	}
	if got := Prior(nil, "smith"); got != FirstObservation {
		t.Fatalf("no prior snapshot = %q", got)
	}
}

func TestCountsGroupUnknownWithUntracked(t *testing.T) {
	s := &Snapshot{Agents: []Assignment{
		{Capacity: Healthy}, {Capacity: Exhausted}, {Capacity: AtRisk},
		{Capacity: Unknown}, {Capacity: Untracked},
	}}
	c := s.Counts()
	if c.Agents != 5 || c.Exhausted != 1 || c.AtRisk != 1 || c.Unknown != 2 {
		t.Fatalf("counts = %+v", c)
	}
}
