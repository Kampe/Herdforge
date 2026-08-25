package pulse

import (
	"encoding/json"
	"strings"
	"testing"
)

// FAC-624: Pending counts ledger pending ROWS. That is correct for its
// definition and reported 0 while 88 un-ingested verdicts sat in the inbox. An
// observer read "pending=0, known=true" as "no review work waiting" -- a fair
// reading of that name, and wrong. The backlog that actually needs draining must
// be visible in the same observation, or nothing surfaces it.
func TestReviewObservation_ExposesInboxBacklogSeparatelyFromPending(t *testing.T) {
	obs := ReviewObservation{Known: true, Pending: 0, InboxUningested: 88}
	raw, err := json.Marshal(obs)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	if !strings.Contains(body, `"inbox_uningested":88`) {
		t.Fatalf("inbox backlog must be serialized so a machine consumer sees it: %s", body)
	}
	if !strings.Contains(body, `"pending":0`) {
		t.Fatalf("pending must remain distinct from the inbox backlog: %s", body)
	}
}

// The two counts measure different things and must never be conflated: a zero
// ledger-pending with a non-zero inbox is the exact state that misled a reader.
func TestReviewObservation_ZeroPendingWithBacklogIsRepresentable(t *testing.T) {
	obs := ReviewObservation{Known: true, Pending: 0, InboxUningested: 88}
	if obs.Pending == obs.InboxUningested {
		t.Fatal("pending and inbox backlog must be independently representable")
	}
}
