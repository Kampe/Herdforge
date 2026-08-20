package control

import "testing"

// The wake prompt is read by an agent, not by the control plane. It used to be
// the bare protocol string "consume durable control envelope <id> seq <n>",
// which stalled the FAC-103 lane: the agent could not resolve the envelope ID
// to any CLI verb, refused to act on it, and stopped on a clarifying question
// with its packet unread.
//
// BOTH wake call sites are covered. The dispatch-side text was a hand-copied
// second copy in the first version of this change, so the invariant held on one
// call site while the other could regress to a bare directive with a green
// suite. They now share wakeTextWithReference and are asserted together.
func TestEveryWakeDirectsTheAgentToItsPacketAndKeepsProvenance(t *testing.T) {
	cases := map[string]struct {
		text     string
		provenan []string
	}{
		"durable control envelope": {wakeText("control-abc123", 21), []string{"control-abc123", "seq 21"}},
		"dispatch task ref":        {WakeTextForTask("FAC-204"), []string{"FAC-204"}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if !contains(tc.text, "TASK-PACKET.md") {
				t.Errorf("wake does not name the packet: %s", tc.text)
			}
			for _, want := range tc.provenan {
				if !contains(tc.text, want) {
					t.Errorf("wake lost provenance %q: %s", want, tc.text)
				}
			}
			for _, want := range []string{"ADDRESSED ASSIGNMENT", "issuer: coordinator", "NOT AUTOMATED STOP-HOOK OUTPUT"} {
				if !contains(tc.text, want) {
					t.Errorf("wake does not identify addressed assignment provenance %q: %s", want, tc.text)
				}
			}
			// The regression was the message READING as an order to consume an
			// envelope. Reject the verb ANYWHERE, not as a prefix, so merely
			// moving it does not slip past.
			if contains(tc.text, "consume") || contains(tc.text, "Consume") {
				t.Errorf("wake tells the agent to consume something again: %s", tc.text)
			}
			// The old string was 52 characters of protocol. A wake with room for
			// only an ID is a wake with no instruction in it.
			if len(tc.text) < 80 {
				t.Errorf("wake too short to carry an instruction (%d chars): %s", len(tc.text), tc.text)
			}
		})
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
