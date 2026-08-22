package pulse

import (
	"reflect"
	"testing"
)

// TestProjectAgentCopiesEveryField is the mechanical gate for the FAC-566
// follow-up.
//
// The projection was an inline literal copying 19 of 20 fields. The dropped one
// was Worktree, so OpenReview received an empty path and every handoff said
// "CANDIDATES: UNRESOLVED — worktree dir required" while Herdr was reporting the
// correct cwd. My packet test passed a lane with Worktree already set, so it
// proved the formatter and never the path.
//
// This asserts by reflection that no field is left behind. Adding a field to
// AgentObservation cannot silently vanish here again -- which is a gate, not a
// promise to remember.
func TestProjectAgentCopiesEveryField(t *testing.T) {
	var in AgentObservation
	v := reflect.ValueOf(&in).Elem()
	typ := v.Type()

	// Fill every field with a distinctive non-zero value.
	for i := 0; i < v.NumField(); i++ {
		f := v.Field(i)
		if !f.CanSet() {
			continue
		}
		switch f.Kind() {
		case reflect.String:
			f.SetString("set-" + typ.Field(i).Name)
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			f.SetUint(7)
		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			f.SetInt(7)
		}
	}

	out := projectAgent(in, "lane", StatusBusy, "busy")

	// Name, Status, Raw and Stale are deliberately overridden by classification.
	overridden := map[string]bool{"Name": true, "Status": true, "Raw": true, "Stale": true}
	ov := reflect.ValueOf(out)
	for i := 0; i < ov.NumField(); i++ {
		name := typ.Field(i).Name
		if overridden[name] {
			continue
		}
		if ov.Field(i).IsZero() {
			t.Fatalf("projection dropped field %q — a field-by-field copy fails silently; "+
				"copy the whole value and override only what classification changes", name)
		}
	}
}

// The overrides themselves must still apply, or classification would be lost.
func TestProjectAgentAppliesClassification(t *testing.T) {
	out := projectAgent(AgentObservation{Name: "old", Raw: "old"}, "lane", StatusStale, "stale-raw")
	if out.Name != "lane" || out.Raw != "stale-raw" || out.Status != StatusStale {
		t.Fatalf("classification must be applied, got %+v", out)
	}
	if !out.Stale {
		t.Fatal("a stale classification must set Stale")
	}
}

// TestWorktreeReachesTheSnapshot is the behavioural half: the field must survive
// into snap.Agents, which is what Apply indexes.
func TestWorktreeReachesTheSnapshot(t *testing.T) {
	out := projectAgent(AgentObservation{Worktree: "/wt/api-crusader"}, "forge-api-crusader", StatusDone, "done")
	if out.Worktree != "/wt/api-crusader" {
		t.Fatalf("worktree must survive projection, got %q", out.Worktree)
	}
}
