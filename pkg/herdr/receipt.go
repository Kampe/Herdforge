package herdr

import (
	"fmt"
	"time"
)

// PromptReceipt proves a prompt was submitted and the agent left its baseline
// status with a prompt-correlated sequence change (FAC-121).
// Consumed is true only when FinalStatus is a consumption state (working/done)
// AND it differs from BaselineStatus — working→working or done→done is not proof.
type PromptReceipt struct {
	Target         string        `json:"target"`
	BaselineStatus string        `json:"baseline_status"`
	FinalStatus    string        `json:"final_status"`
	Consumed       bool          `json:"consumed"`
	Verified       bool          `json:"verified"`
	Duration       time.Duration `json:"duration"`
	SequenceToken  string        `json:"sequence_token"` // baseline->final for outbox correlation
}

// ConsumptionProven reports whether observed status proves the prompt was
// consumed relative to the recorded baseline (FAC-121 R3 repair).
//
// Rules:
//   - observed must be working or done (agent accepted work)
//   - observed must differ from baseline (sequence change)
//   - working→working and done→done are explicitly rejected
func ConsumptionProven(baseline, observed string) bool {
	return ConsumptionProvenSeen(baseline, observed, false)
}

// ConsumptionProvenSeen is ConsumptionProven with the extra evidence of
// whether the agent was ever observed WORKING during this delivery.
//
// A bare idle->done transition is NOT proof. A freshly launched agent renders
// its UI and can settle straight to done without ever processing the prompt —
// observed twice on grok lanes (FAC-174, FAC-172), each time reporting a
// healthy delivery while the pane sat at an empty prompt with only its
// dispatch anchor commit. A lane that looks busy while idle is worse than one
// that fails loudly, so done only counts when the agent actually passed
// through working, or was already working when the prompt arrived (the repair
// case, where a busy agent takes follow-up work).
func ConsumptionProvenSeen(baseline, observed string, sawWorking bool) bool {
	if observed == baseline {
		return false
	}
	switch observed {
	case "working":
		return true
	case "done":
		return sawWorking || baseline == "working"
	}
	return false
}

// DeliverAndProve submits text and polls until the agent confirms consumption
// via a state/sequence change from the pre-submit baseline.
// A submit into a dead pane, or an already-working/done agent that never
// transitions, returns an error so dispatch can compensate.
func DeliverAndProve(target, text string, timeout time.Duration) (*PromptReceipt, error) {
	start := time.Now()
	baseline, _ := liveStatus(target) // empty if agent not yet listed

	if _, err := AgentPrompt(target, text, false); err != nil {
		return &PromptReceipt{
			Target:         target,
			BaselineStatus: baseline,
			Consumed:       false,
			Verified:       false, // never "verified" when the prompt never reached a pane
			Duration:       time.Since(start),
			SequenceToken:  sequenceToken(baseline, ""),
		}, err
	}

	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	poll := 2 * time.Second
	deadline := time.Now().Add(timeout)
	nudged := false
	last := baseline
	sawWorking := baseline == "working"
	for time.Now().Before(deadline) {
		st, err := liveStatus(target)
		if err == nil {
			last = st
			if st == "working" {
				sawWorking = true
			}
			if ConsumptionProvenSeen(baseline, st, sawWorking) {
				return &PromptReceipt{
					Target:         target,
					BaselineStatus: baseline,
					FinalStatus:    st,
					Consumed:       true,
					Verified:       true,
					Duration:       time.Since(start),
					SequenceToken:  sequenceToken(baseline, st),
				}, nil
			}
		}
		if !nudged && time.Now().Add(poll).After(deadline.Add(-timeout/2)) {
			_ = SendKeys(target, "Enter")
			nudged = true
		}
		time.Sleep(poll)
	}
	return &PromptReceipt{
		Target:         target,
		BaselineStatus: baseline,
		FinalStatus:    last,
		Consumed:       false,
		Verified:       false,
		Duration:       time.Since(start),
		SequenceToken:  sequenceToken(baseline, last),
	}, fmt.Errorf("agent %q never confirmed prompt-correlated consumption (baseline %q last %q; working→working/done→done and a bare idle→done are not proof)", target, baseline, last)
}

func sequenceToken(baseline, final string) string {
	return fmt.Sprintf("%s->%s", baseline, final)
}
