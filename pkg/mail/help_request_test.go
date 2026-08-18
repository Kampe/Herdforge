package mail

import (
	"encoding/json"
	"testing"
)

func TestPostHelpRequestFanoutAndDeduplication(t *testing.T) {
	mb := NewMailbox(t.TempDir() + "/mail.jsonl")
	req := HelpRequest{
		Lane: "worker-1", TaskRef: "FAC-371", Reason: "review capacity unavailable",
		Capability: "review", SuggestedHelper: "review-supervisor", SuggestedFamily: "google",
	}
	first, err := mb.PostHelpRequest("worker-1", req)
	if err != nil {
		t.Fatal(err)
	}
	wantID := HelpRequestID(req)
	if len(first) != 4 || first[0].ID[:len(wantID)] != wantID {
		t.Fatalf("request=%q envelopes=%d, want stable request and lane/helper/supervisor/coordinator fanout", wantID, len(first))
	}
	second, err := mb.PostHelpRequest("worker-1", req)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != len(first) {
		t.Fatalf("retry returned %d envelopes, want %d", len(second), len(first))
	}
	all, err := mb.ReadInbox("")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(first) {
		t.Fatalf("durable fanout has %d records, want %d", len(all), len(first))
	}
	for _, env := range all {
		var got HelpRequest
		if err := json.Unmarshal([]byte(env.Body), &got); err != nil {
			t.Fatal(err)
		}
		if got.RequestID != wantID {
			t.Fatalf("request id %q, want %q", got.RequestID, wantID)
		}
	}
}

func TestHelpRequestIDChangesWhenBlockedReasonChanges(t *testing.T) {
	base := HelpRequest{Lane: "worker-1", TaskRef: "FAC-371", Reason: "quota", Capability: "implementation"}
	other := base
	other.Reason = "credentials"
	if HelpRequestID(base) == HelpRequestID(other) {
		t.Fatal("different blocked reasons must not deduplicate")
	}
}
