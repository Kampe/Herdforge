package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/timeline"
)

func TestTimelineCommandFiltersChronologicalEnvelopeRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeline.jsonl")
	store, err := timeline.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range []timeline.Envelope{
		{Version: timeline.Version1, ID: "two", BuildRun: "run", Task: "FAC-2", Attempt: "1", Lane: "build", Session: "s", Model: "m", Provider: "p", Source: "lifecycle", Type: "transition.claimed", Time: time.Date(2026, 8, 13, 12, 1, 0, 0, time.UTC), Evidence: "sha256:2", Correlation: timeline.CorrelationKnown},
		{Version: timeline.Version1, ID: "one", BuildRun: "run", Task: "FAC-1", Attempt: "1", Lane: "build", Session: "s", Model: "m", Provider: "p", Source: "lifecycle", Type: "transition.claimed", Time: time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC), Evidence: "sha256:1", Correlation: timeline.CorrelationKnown},
	} {
		if err := store.Append(event); err != nil {
			t.Fatal(err)
		}
	}
	var out, errOut bytes.Buffer
	if code := runTimelineCommand([]string{"--file", path, "--lane", "build"}, &out, &errOut); code != 0 {
		t.Fatalf("code=%d stderr=%s", code, errOut.String())
	}
	var got []timeline.Envelope
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "one" || got[1].ID != "two" {
		t.Fatalf("timeline = %+v", got)
	}
}
