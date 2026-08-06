package watch

import (
	"strings"
	"testing"
)

func TestSettledStatuses(t *testing.T) {
	for _, s := range []string{"idle", "done", "blocked", "unknown", ""} {
		if !Settled(s) {
			t.Fatalf("%q must count as settled", s)
		}
	}
	for _, s := range []string{"working", "starting", "WORKING"} {
		if Settled(s) {
			t.Fatalf("%q is still active", s)
		}
	}
}

// Status flapping must not fire a false harvest.
func TestDebounceRequiresTwoConsecutivePolls(t *testing.T) {
	s := NewState()
	if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 0 {
		t.Fatalf("one non-working poll is not a settle: %v", ev)
	}
	ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}})
	if len(ev) != 1 || ev[0].PaneID != "p1" {
		t.Fatalf("two consecutive polls must fire: %v", ev)
	}
}

func TestFlapResetsTheDebounce(t *testing.T) {
	s := NewState()
	s.Poll([]Observation{{PaneID: "p1", Status: "idle"}})
	// Flap back to working: the pending settle must be discarded.
	s.Poll([]Observation{{PaneID: "p1", Status: "working"}})
	if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 0 {
		t.Fatalf("debounce must restart after a flap, got %v", ev)
	}
	if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 1 {
		t.Fatalf("settle must fire after two clean polls, got %v", ev)
	}
}

// A settle fires once, not every poll thereafter.
func TestSettleFiresOncePerWorkCycle(t *testing.T) {
	s := NewState()
	s.Poll([]Observation{{PaneID: "p1", Status: "idle"}})
	if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 1 {
		t.Fatal("expected first fire")
	}
	for i := 0; i < 3; i++ {
		if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 0 {
			t.Fatalf("a settled pane must not re-fire every poll: %v", ev)
		}
	}
	// New work, then settling again, IS a new event.
	s.Poll([]Observation{{PaneID: "p1", Status: "working"}})
	s.Poll([]Observation{{PaneID: "p1", Status: "idle"}})
	if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 1 {
		t.Fatalf("a second work cycle must fire again: %v", ev)
	}
}

// Panes appearing mid-wave must be picked up, which is why the caller
// re-enumerates every poll.
func TestPanesAppearingMidWaveAreTracked(t *testing.T) {
	s := NewState()
	s.Poll([]Observation{{PaneID: "p1", Status: "working"}})
	// A reviewer spawned after the watch started.
	s.Poll([]Observation{{PaneID: "p1", Status: "working"}, {PaneID: "p2", Status: "idle"}})
	ev := s.Poll([]Observation{{PaneID: "p1", Status: "working"}, {PaneID: "p2", Status: "idle"}})
	if len(ev) != 1 || ev[0].PaneID != "p2" {
		t.Fatalf("mid-wave pane must settle: %v", ev)
	}
}

// Stale state for a closed tab must not fire when an ID is reused.
func TestVanishedPanesDropTheirState(t *testing.T) {
	s := NewState()
	s.Poll([]Observation{{PaneID: "p1", Status: "idle"}})
	s.Poll(nil) // tab closed
	if ev := s.Poll([]Observation{{PaneID: "p1", Status: "idle"}}); len(ev) != 0 {
		t.Fatalf("reused pane id must restart debounce, got %v", ev)
	}
}

func TestAllSettledGatesOnEveryNamedPane(t *testing.T) {
	s := NewState()
	obs := []Observation{{PaneID: "p1", Status: "idle"}, {PaneID: "p2", Status: "working"}}
	s.Poll(obs)
	s.Poll(obs)
	if s.AllSettled([]string{"p1", "p2"}) {
		t.Fatal("one pane still working means not all settled")
	}
	done := []Observation{{PaneID: "p1", Status: "idle"}, {PaneID: "p2", Status: "idle"}}
	s.Poll(done)
	s.Poll(done)
	if !s.AllSettled([]string{"p1", "p2"}) {
		t.Fatal("both settled must satisfy --all")
	}
	if s.AllSettled(nil) {
		t.Fatal("an empty pane list is not 'all settled'")
	}
}

// Naming only the settled pane trained the coordinator into narrow harvests.
func TestSettleLineDemandsAFullHarvest(t *testing.T) {
	line := SettleLine(Event{PaneID: "wK:p2", Name: "smith", Status: "idle"}, 3)
	for _, want := range []string{"SETTLED wK:p2=idle", "name=smith", "attention=3", "HARVEST ALL"} {
		if !strings.Contains(line, want) {
			t.Fatalf("settle line missing %q: %s", want, line)
		}
	}
}
