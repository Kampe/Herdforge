package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeJSONL(t *testing.T, path string, rows ...LedgerRow) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range rows {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLedgerContract_StrictAndOrdering(t *testing.T) {
	tests := []struct {
		name     string
		rows     []LedgerRow
		wantPass map[string]string
		wantVeto map[string]bool
	}{
		{name: "record tier fallback and superseded fail", rows: []LedgerRow{
			{Event: "record", SHA: "a", Reviewer: "r", Tier: "R2", BuilderFamily: "anthropic"},
			{Event: "verdict", SHA: "a", Reviewer: "r", Verdict: "FAIL"},
			{Event: "verdict", SHA: "a", Reviewer: "r", Verdict: "PASS"},
			{Event: "record", SHA: "b", Reviewer: "r", Tier: ""},
			{Event: "verdict", SHA: "b", Reviewer: "r", Verdict: "PASS", Tier: ""},
			{Event: "record", SHA: "c", Reviewer: "r", Tier: "R1"},
			{Event: "verdict", SHA: "c", Reviewer: "r", Verdict: "PASS", Tier: "R0"},
			{Event: "verdict", SHA: "d", Reviewer: "one", Verdict: "FAIL"},
			{Event: "verdict", SHA: "d", Reviewer: "two", Verdict: "PASS"},
		}, wantPass: map[string]string{"a": "R2", "b": "", "c": "R0", "d": ""}, wantVeto: map[string]bool{"d": true}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "ledger.jsonl")
			writeJSONL(t, path, tc.rows...)
			l := OpenLedger(path)
			passes, err := l.PASSes(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for sha, want := range tc.wantPass {
				if got := passes[sha]; got != want {
					t.Errorf("PASSes[%s]=%q want %q", sha, got, want)
				}
			}
			veto, err := l.Vetoed(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(veto) != len(tc.wantVeto) {
				t.Fatalf("Vetoed=%v want %v", veto, tc.wantVeto)
			}
			for sha := range tc.wantVeto {
				if !veto[sha] {
					t.Errorf("missing veto %s", sha)
				}
			}
		})
	}
}

func TestLedgerContract_MalformedJSONLIsHardError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ledger.jsonl")
	if err := os.WriteFile(path, []byte(`{"event":"record"}
not-json
`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLedger(path).Snapshot(); err == nil {
		t.Fatal("malformed ledger must fail closed")
	}
}

func TestLedgerContract_QueueConsumeAndLaterRecordRecovery(t *testing.T) {
	dir := t.TempDir()
	l := OpenLedger(filepath.Join(dir, "ledger.jsonl"))
	writeJSONL(t, l.Path, LedgerRow{Event: "record", SHA: "abc", Branch: "task/FAC-65"})
	writeJSONL(t, l.QueuePath, LedgerRow{Event: "enqueue", SHA: "abc", Status: "queued"})
	s := mustSnapshot(t, l)
	pins := queuePins(s, nil, map[string]bool{})
	if len(pins) != 1 || pins[0].branch != "task/FAC-65" {
		t.Fatalf("recovered pins=%+v", pins)
	}
	writeJSONL(t, l.QueuePath, LedgerRow{Event: "enqueue", SHA: "abc", Status: "queued"}, LedgerRow{Event: "consumed", SHA: "abc", Status: "consumed"})
	s = mustSnapshot(t, l)
	if got := queuePins(s, nil, map[string]bool{}); len(got) != 0 {
		t.Fatalf("consumed pin reappeared: %+v", got)
	}
}

func mustSnapshot(t *testing.T, l *Ledger) LedgerSnapshot {
	t.Helper()
	s, e := l.Snapshot()
	if e != nil {
		t.Fatal(e)
	}
	return s
}
