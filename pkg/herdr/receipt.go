package herdr

import (
	"fmt"
	"time"
)

// PromptReceipt proves a prompt was submitted and the agent left its baseline
// status (FAC-121 sequence baseline/receipt). Consumed is true only when the
// observed final status differs from the baseline in a way that indicates the
// agent accepted work (working/done) or, when verify is disabled, when submit
// itself succeeded.
type PromptReceipt struct {
	Target          string        `json:"target"`
	BaselineStatus  string        `json:"baseline_status"`
	FinalStatus     string        `json:"final_status"`
	Consumed        bool          `json:"consumed"`
	Verified        bool          `json:"verified"`
	Duration        time.Duration `json:"duration"`
	SequenceToken   string        `json:"sequence_token"` // baseline|final for outbox correlation
}

// DeliverAndProve submits text and, when verify is true, polls until the agent
// confirms consumption (working/done) relative to a pre-submit baseline status.
// A submit into a dead pane that never flips status returns an error so dispatch
// can compensate (close tab, recover task) instead of claiming success.
func DeliverAndProve(target, text string, verify bool, timeout time.Duration) (*PromptReceipt, error) {
	start := time.Now()
	baseline, _ := liveStatus(target) // empty if agent not yet listed

	if _, err := AgentPrompt(target, text, false); err != nil {
		return &PromptReceipt{
			Target:         target,
			BaselineStatus: baseline,
			Consumed:       false,
			Verified:       verify,
			Duration:       time.Since(start),
			SequenceToken:  sequenceToken(baseline, ""),
		}, err
	}

	if !verify {
		return &PromptReceipt{
			Target:         target,
			BaselineStatus: baseline,
			FinalStatus:    "submitted",
			Consumed:       true,
			Verified:       false,
			Duration:       time.Since(start),
			SequenceToken:  sequenceToken(baseline, "submitted"),
		}, nil
	}

	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	poll := 2 * time.Second
	deadline := time.Now().Add(timeout)
	nudged := false
	last := baseline
	for time.Now().Before(deadline) {
		st, err := liveStatus(target)
		if err == nil {
			last = st
			if st == "working" || st == "done" {
				rec := &PromptReceipt{
					Target:         target,
					BaselineStatus: baseline,
					FinalStatus:    st,
					Consumed:       true,
					Verified:       true,
					Duration:       time.Since(start),
					SequenceToken:  sequenceToken(baseline, st),
				}
				return rec, nil
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
		Verified:       true,
		Duration:       time.Since(start),
		SequenceToken:  sequenceToken(baseline, last),
	}, fmt.Errorf("agent %q never confirmed prompt consumption (baseline %q last %q)", target, baseline, last)
}

func sequenceToken(baseline, final string) string {
	return fmt.Sprintf("%s->%s", baseline, final)
}
