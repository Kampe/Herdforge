package feedback

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestMissingIsSetDifferenceAndDeterministic(t *testing.T) {
	got := Missing([]string{"smith", "scout", "assayer"}, []string{"scout"})
	want := []string{"assayer", "smith"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("missing = %v, want %v", got, want)
	}
	if all := Missing([]string{"a"}, []string{"a"}); len(all) != 0 {
		t.Fatalf("fully answered census must report nothing missing, got %v", all)
	}
}

func TestDueAndOverdueWindows(t *testing.T) {
	now := time.Unix(1_800_000_000, 0).UTC()
	if !Due(time.Time{}, now, DefaultInterval) {
		t.Fatal("no prior census must be due")
	}
	if Due(now.Add(-5*time.Minute), now, DefaultInterval) {
		t.Fatal("a census inside the interval must not reopen")
	}
	if !Due(now.Add(-31*time.Minute), now, DefaultInterval) {
		t.Fatal("a census past the interval must reopen")
	}
	if Overdue(now.Add(-time.Hour), now, DefaultGrace, 0) {
		t.Fatal("zero missing is never overdue")
	}
	if Overdue(now.Add(-time.Minute), now, DefaultGrace, 2) {
		t.Fatal("inside grace is not overdue")
	}
	if !Overdue(now.Add(-11*time.Minute), now, DefaultGrace, 2) {
		t.Fatal("missing replies past grace must be overdue")
	}
}

// A corrupt census must fail closed. Treating it as a fresh start would drop
// the outstanding request set and report a false all-clear.
func TestCorruptCensusFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(StatePath(dir), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("corrupt census must not silently become an empty one")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested")
	want := &State{Epoch: "20260806T000000Z", RequestedAtEpoch: 42, Lanes: []string{"smith"}}
	if err := Save(dir, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if missing, err := Load(t.TempDir()); err != nil || missing.Epoch != "" {
		t.Fatalf("absent census must load empty: %+v %v", missing, err)
	}
}

// Active agents must not be interrupted; settled ones need the nudge.
func TestNeedsWakeOnlyForSettledAgents(t *testing.T) {
	for _, s := range []string{"idle", "done", "blocked", "unknown", ""} {
		if !NeedsWake(s) {
			t.Fatalf("settled status %q must be woken", s)
		}
	}
	for _, s := range []string{"working", "starting"} {
		if NeedsWake(s) {
			t.Fatalf("active status %q must not be interrupted", s)
		}
	}
}

func TestRequestBodyNamesTheCountableReplyShape(t *testing.T) {
	body := RequestBody("E1", "orchestrator")
	for _, want := range []string{"FLEET_FEEDBACK E1", "herd mail send orchestrator", "NONE"} {
		if !contains(body, want) {
			t.Fatalf("request body missing %q: %s", want, body)
		}
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && (func() bool {
		for i := 0; i+len(needle) <= len(hay); i++ {
			if hay[i:i+len(needle)] == needle {
				return true
			}
		}
		return false
	})()
}
