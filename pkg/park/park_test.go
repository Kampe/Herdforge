package park

import "testing"

func TestIsParkedSubjectMatchesDeliberatelyUnfinishedWork(t *testing.T) {
	for _, s := range []string{
		"wip(parked): half-done router split",
		"WIP: still shaping this",
		"parked: waiting on FAC-99",
	} {
		if !IsParkedSubject(s) {
			t.Fatalf("%q must be recognised as parked", s)
		}
	}
	for _, s := range []string{
		"fix(router): unpin builders",
		"feat: add posture",
		"chore: tidy",
		"",
	} {
		if IsParkedSubject(s) {
			t.Fatalf("%q is ordinary work, not parked", s)
		}
	}
}

// The three durability levels are exactly the three failure modes: no tag dies
// to a reset, a local-only tag dies to a lost checkout, a pushed tag survives.
func TestClassifyDurability(t *testing.T) {
	if got := Classify(nil, false); got != Exposed {
		t.Fatalf("branch-only must be EXPOSED, got %s", got)
	}
	if got := Classify([]string{"parked/x"}, false); got != LocalTagOnly {
		t.Fatalf("unpushed tag must be LOCAL-TAG-ONLY, got %s", got)
	}
	if got := Classify([]string{"parked/x"}, true); got != Durable {
		t.Fatalf("pushed tag must be DURABLE, got %s", got)
	}
	// A pushed flag with no tag is incoherent input; exposure must still win
	// rather than reporting durable off a phantom.
	if got := Classify(nil, true); got != Exposed {
		t.Fatalf("no tag can never be durable, got %s", got)
	}
}

func TestExposedCountDrivesTheNonZeroExit(t *testing.T) {
	findings := []Finding{
		{Durability: Durable},
		{Durability: Exposed},
		{Durability: LocalTagOnly},
	}
	if n := ExposedCount(findings); n != 2 {
		t.Fatalf("exposed count = %d, want 2", n)
	}
	if n := ExposedCount([]Finding{{Durability: Durable}}); n != 0 {
		t.Fatalf("all-durable must report zero, got %d", n)
	}
	if n := ExposedCount(nil); n != 0 {
		t.Fatalf("no parked work must report zero, got %d", n)
	}
}
