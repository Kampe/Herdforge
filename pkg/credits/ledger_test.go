package credits

import (
	"os"
	"path/filepath"
	"strings"
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

func TestWriteMutation_RenameFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")

	l, _ := OpenLedger(p)
	err := l.WriteMutation(func(m *map[string]Record) {
		(*m)["claude"] = Record{UsedPct: 42, WindowDays: intPtr(7), DaysLeft: intPtr(5)}
	})
	if err != nil {
		t.Fatalf("seed WriteMutation: %v", err)
	}

	// replace the ledger file with a directory of the same name so rename fails
	if err := os.Remove(p); err != nil {
		t.Fatalf("remove ledger: %v", err)
	}
	if err := os.Mkdir(p, 0755); err != nil {
		t.Fatalf("mkdir collision: %v", err)
	}
	defer func() {
		os.Remove(p) // remove the directory so TempDir cleanup works
	}()

	err = l.WriteMutation(func(m *map[string]Record) {
		data := *m
		rec := data["claude"]
		rec.UsedPct = 77
		*rec.DaysLeft = 1
		data["claude"] = rec
	})
	if err == nil {
		t.Fatal("expected rename failure error")
	}

	// committed state must not expose the unpersisted mutation — including
	// through aliased WindowDays/DaysLeft pointers
	rec, serr := l.Surface("claude")
	if serr != nil {
		t.Fatalf("Surface after failed mutation: %v", serr)
	}
	if rec.UsedPct != 42 {
		t.Errorf("failed mutation must not change committed state: got %d, want 42", rec.UsedPct)
	}
	if rec.DaysLeft == nil || *rec.DaysLeft != 5 {
		t.Errorf("failed mutation must not change committed DaysLeft: got %v, want 5", rec.DaysLeft)
	}
	if rec.WindowDays == nil || *rec.WindowDays != 7 {
		t.Errorf("failed mutation must not change committed WindowDays: got %v, want 7", rec.WindowDays)
	}

	// tmp artifact must be removed
	if _, statErr := os.Stat(p + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("tmp file must be removed after failed mutation, stat err: %v", statErr)
	}
}

func TestWriteMutation_WriteFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")

	l, _ := OpenLedger(p)
	err := l.WriteMutation(func(m *map[string]Record) {
		(*m)["claude"] = Record{UsedPct: 42}
	})
	if err != nil {
		t.Fatalf("seed WriteMutation: %v", err)
	}

	// make the ledger directory read-only so the tmp write fails
	if err := os.Chmod(dir, 0555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	defer os.Chmod(dir, 0755)

	err = l.WriteMutation(func(m *map[string]Record) {
		data := *m
		rec := data["claude"]
		rec.UsedPct = 77
		data["claude"] = rec
	})
	if err == nil {
		t.Fatal("expected write failure error")
	}

	rec, serr := l.Surface("claude")
	if serr != nil {
		t.Fatalf("Surface after failed mutation: %v", serr)
	}
	if rec.UsedPct != 42 {
		t.Errorf("failed mutation must not change committed state: got %d, want 42", rec.UsedPct)
	}

	if _, statErr := os.Stat(p + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("tmp file must be removed after failed mutation, stat err: %v", statErr)
	}
}

func TestWriteMutation_PreservesAccountSourceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "ledger.json")

	// shell-produced ledger shape with per-account source
	os.WriteFile(p, []byte(`{"claude":{"used_pct":50,"accounts":[{"email":"a@x","used_pct":50,"burn_order":1,"note":"","updated":1,"source":"ccusage"}]}}`), 0644)

	l, err := OpenLedger(p)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 1 || rec.Accounts[0].Source != "ccusage" {
		t.Fatalf("source field must decode, got %+v", rec.Accounts)
	}

	// no-op mutation must not drop the field on re-encode
	if err := l.WriteMutation(func(m *map[string]Record) {}); err != nil {
		t.Fatalf("no-op WriteMutation: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read re-encoded ledger: %v", err)
	}
	if !strings.Contains(string(raw), `"source":"ccusage"`) && !strings.Contains(string(raw), `"source": "ccusage"`) {
		t.Errorf("source field must survive re-encode, got: %s", string(raw))
	}
}
