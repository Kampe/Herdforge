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
	t.Run("resolved dependency", func(t *testing.T) {
		// FAC-581 correction (finding 1): a closed blocker admits the
		// dependent. The caller passes the authoritative closed set.
		d := pulseDispatchDecision([]*provider.Task{
			{
				Ref: "FAC-75", ID: "t1", Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
				Description: herdDepsFence("FAC-75", "t1", `[{"source_ref":"FAC-136","target_ref":"FAC-75","type":"blocks"}]`),
			},
		}, broker.Inputs{ClosedTasks: map[string]bool{"FAC-136": true}})
		if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-75" {
			t.Fatalf("resolved dependency must admit the dependent: %+v", d)
		}
	})
	t.Run("invalid provenance fail closed", func(t *testing.T) {
		// FAC-581 correction (finding 2): a malformed fence is unknown
		// dependency state — reported as a named block, never a ready task.
		d := pulseDispatchDecision([]*provider.Task{
			{
				Ref: "FAC-9", ID: "t9", Status: provider.StatusToDo, Priority: provider.PriorityUrgent,
				Description: "```herd-deps-v1\n{malformed}\n```\n",
			},
		}, broker.Inputs{})
		if d.Outcome != broker.OutcomeWait || !strings.Contains(d.WaitReason, "invalid herd-deps-v1 provenance") {
			t.Fatalf("malformed provenance must fail closed with a named wait: %+v", d)
		}
	})
	t.Run("numeric ref order", func(t *testing.T) {
		// FAC-581 correction (finding 3): equal-priority refs order by numeric
		// ticket number (FAC-3 before FAC-10), the repo claim-order invariant.
		d := pulseDispatchDecision(nil, broker.Inputs{
			Accepts: []broker.Kind{broker.KindBuild},
			Queue: []broker.Task{
				{Ref: "FAC-10", Kind: broker.KindBuild, Priority: 5},
				{Ref: "FAC-3", Kind: broker.KindBuild, Priority: 5},
			},
		})
		if d.Outcome != broker.OutcomeWork || d.Task == nil || d.Task.Ref != "FAC-3" {
			t.Fatalf("equal priority must select the numerically lowest ref: %+v", d.Task)
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
