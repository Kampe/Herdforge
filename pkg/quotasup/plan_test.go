package quotasup

import (
	"reflect"
	"testing"
)

func TestPlanLanes(t *testing.T) {
	cases := []struct {
		name      string
		queue     int
		decisions []Decision
		roster    []string
		want      LanePlan
	}{
		{"all pools healthy", 4, []Decision{{Surface: Surface{Provider: "claude"}, Cap: 2}, {Surface: Surface{Provider: "codex"}, Cap: 3}}, []string{"codex", "claude"}, LanePlan{Desired: 4, Raises: []LaneRaise{{Provider: "claude", Count: 2}, {Provider: "codex", Count: 2}}}},
		{"one pool exhausted", 4, []Decision{{Surface: Surface{Provider: "claude"}, Cap: 0}, {Surface: Surface{Provider: "codex"}, Cap: 2}}, []string{"claude", "codex"}, LanePlan{Desired: 2, Raises: []LaneRaise{{Provider: "codex", Count: 2}}}},
		{"all pools exhausted", 4, []Decision{{Surface: Surface{Provider: "claude"}}, {Surface: Surface{Provider: "codex"}}}, []string{"claude", "codex"}, LanePlan{}},
		{"queue empty", 0, []Decision{{Surface: Surface{Provider: "codex"}, Cap: 3, Evidence: Evidence{Active: 1}}}, []string{"codex"}, LanePlan{}},
		{"non-admitting surface", 2, []Decision{{Surface: Surface{Provider: "codex"}, Cap: 3, Evidence: Evidence{Active: 3}}}, []string{"codex"}, LanePlan{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PlanLanes(tc.queue, tc.decisions, tc.roster); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("PlanLanes() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestPlanLanesIsDeterministic(t *testing.T) {
	decisions := []Decision{{Surface: Surface{Provider: "grok"}, Cap: 2}, {Surface: Surface{Provider: "codex"}, Cap: 2}}
	want := PlanLanes(3, decisions, []string{"grok", "codex"})
	for i := 0; i < 20; i++ {
		if got := PlanLanes(3, decisions, []string{"codex", "grok"}); !reflect.DeepEqual(got, want) {
			t.Fatalf("iteration %d: got %+v, want %+v", i, got, want)
		}
	}
}
