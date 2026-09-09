package main

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/broker"
	"github.com/Kampe/Herdforge/pkg/progress"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// FAC-581 broker conformance fixture: the hermetic record must carry task
// identity, dependency readiness, admission, progress classification, last
// artifact, and an explicit wait reason. The production caller is pulse.
func TestWorkBrokerDecisionConformance(t *testing.T) {
	t.Run("record fields", func(t *testing.T) {
		d := pulseDispatchDecision([]*provider.Task{
			{Ref: "FAC-581", ID: "eow6wtnnj7dm7q159dcuwsz6", Status: provider.StatusToDo, Priority: provider.PriorityUrgent},
		}, broker.Inputs{
			Progress: progress.Record{Lane: "pulse", TaskRef: "FAC-581", Action: progress.ClassBuild, LastArtifact: "sha-builder"},
		})
		if err := d.Validate(); err != nil {
			t.Fatal(err)
		}
		if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref == "" || d.Progress.Action == "" || d.Progress.LastArtifact == "" {
			t.Fatalf("hermetic record missing required fields: %+v", d)
		}
		if d.WaitReason != "" {
			t.Fatalf("ready builder must not wait: %+v", d)
		}
	})
	t.Run("review saturation independent", func(t *testing.T) {
		d := pulseDispatchDecision(nil, broker.Inputs{
			Accepts: []broker.Kind{broker.KindBuild},
			Queue: []broker.Task{
				{Ref: "FAC-581", Kind: broker.KindBuild, Priority: 4},
			},
			ReviewSaturated:  true,
			ReviewWaitReason: "8 reviews in flight >= cap 3",
			Progress:         progress.Record{Lane: "pulse", Action: progress.ClassBuild},
		})
		if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-581" {
			t.Fatalf("full review slot suppressed builder: %+v", d)
		}
	})
	t.Run("dependency blocked", func(t *testing.T) {
		got := selectPulseDispatchTask([]*provider.Task{
			{
				Ref: "FAC-75", ID: "t1", Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
				Description: herdDepsFence("FAC-75", "t1", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
			},
		})
		if got != nil {
			t.Fatalf("open dependency must not dispatch: %+v", got)
		}
		d := pulseDispatchDecision([]*provider.Task{
			{
				Ref: "FAC-75", ID: "t1", Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
				Description: herdDepsFence("FAC-75", "t1", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
			},
		}, broker.Inputs{})
		if d.Outcome != broker.OutcomeWait || !strings.Contains(d.WaitReason, "FAC-136") || d.Blocked["FAC-75"] == "" {
			t.Fatalf("blocked dependency: %+v", d)
		}
	})
	t.Run("event wait", func(t *testing.T) {
		d := pulseDispatchDecision(nil, broker.Inputs{
			Progress: progress.Record{Lane: "pulse", Action: progress.ClassWait, WaitReason: "sleep_is_not_progress"},
		})
		if d.Outcome != broker.OutcomeWait || d.WaitReason != "sleep_is_not_progress" {
			t.Fatalf("sleep scored as work: %+v", d)
		}
	})
	t.Run("missing identity", func(t *testing.T) {
		if err := (broker.Decision{Outcome: broker.OutcomeWork}).Validate(); err == nil {
			t.Fatal("selector output without a task identity must fail")
		}
	})
}
