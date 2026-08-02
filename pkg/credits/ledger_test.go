package credits

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenLedger_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")
	os.WriteFile(p, []byte(`{bad json`), 0644)

	_, err := OpenLedger(p)
	if err == nil {
		t.Fatal("expected error for malformed JSON")
	}
}

func TestOpenLedger_New(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")

	l, err := OpenLedger(p)
	if err != nil {
		t.Fatalf("OpenLedger new: %v", err)
	}
	if l == nil {
		t.Fatal("OpenLedger returned nil")
	}
	if l.Path() != p {
		t.Errorf("path = %s, want %s", l.Path(), p)
	}
	if _, err := l.Surface("claude"); err == nil {
		t.Error("expected error for missing surface")
	}

	all := l.All()
	if len(all) != 0 {
		t.Errorf("expected empty ledger, got %d entries", len(all))
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read created file: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("OpenLedger did not write {} to disk for new file")
	}
}

func TestOpenLedger_Existing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")

	os.WriteFile(p, []byte(`{"claude":{"used_pct":42}}`), 0644)

	l, err := OpenLedger(p)
	if err != nil {
		t.Fatalf("OpenLedger existing: %v", err)
	}

	rec, err := l.Surface("claude")
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if rec.UsedPct != 42 {
		t.Errorf("UsedPct = %d, want 42", rec.UsedPct)
	}
}

func TestWriteMutation(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")

	l, _ := OpenLedger(p)
	err := l.WriteMutation(func(m *map[string]Record) {
		(*m)["claude"] = Record{UsedPct: 50}
	})
	if err != nil {
		t.Fatalf("WriteMutation: %v", err)
	}

	l2, _ := OpenLedger(p)
	rec, _ := l2.Surface("claude")
	if rec.UsedPct != 50 {
		t.Errorf("after reload UsedPct = %d, want 50", rec.UsedPct)
	}
}

func TestEffectiveUsed_Single(t *testing.T) {
	rec := Record{UsedPct: 33}
	got := EffectiveUsed(rec)
	if got != 33.0 {
		t.Errorf("EffectiveUsed(single) = %f, want 33.0", got)
	}
}

func TestEffectiveUsed_Multi(t *testing.T) {
	rec := Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 20},
			{Email: "b@y", UsedPct: 40},
		},
	}
	got := EffectiveUsed(rec)
	if got != 30.0 {
		t.Errorf("EffectiveUsed(multi) = %f, want 30.0", got)
	}
}

func TestPoolNote(t *testing.T) {
	rec := Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 10},
			{Email: "b@y", UsedPct: 30},
		},
		Note: "manual",
	}
	note := PoolNote(rec)
	if note == "" {
		t.Fatal("PoolNote returned empty")
	}
}

func TestSortAccountsByBurnOrder(t *testing.T) {
	rows := []AccountRow{
		{Email: "b", BurnOrder: 2},
		{Email: "c", BurnOrder: 0},
		{Email: "a", BurnOrder: 1},
	}
	SortAccountsByBurnOrder(rows)

	if rows[0].Email != "a" {
		t.Errorf("expected 'a' first, got %s", rows[0].Email)
	}
	if rows[1].Email != "b" {
		t.Errorf("expected 'b' second, got %s", rows[1].Email)
	}
	if rows[2].Email != "c" {
		t.Errorf("expected 'c' third, got %s", rows[2].Email)
	}
}
