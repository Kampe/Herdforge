package credits

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNowEpoch(t *testing.T) {
	e := NowEpoch()
	if e < 1700000000 {
		t.Errorf("NowEpoch seems too small: %d", e)
	}
}

func TestWeekAgoYYYMMDD(t *testing.T) {
	s := WeekAgoYYYMMDD()
	if len(s) != 8 {
		t.Errorf("expected 8 chars, got %s", s)
	}
}

func TestRefresh_SkipNonClaude(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	r := Refresh(l, "openai", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
}

func TestRefresh_SkipNoRefreshEnv(t *testing.T) {
	t.Setenv("HERD_CREDITS_NO_REFRESH", "1")
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
}

func TestMaybeRefresh_SkipAutoNotSet(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	r := MaybeRefresh(l)
	if r != nil {
		t.Fatal("expected nil when HERD_CREDITS_AUTO_REFRESH is not set")
	}
}

func TestLedgerCommands_Show_Empty(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	out := lc.Show()
	if out != "" {
		t.Errorf("expected empty output, got %q", out)
	}
}

func TestLedgerCommands_SetAndPace(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)

	_, err := lc.Set("claude", 42, 7, 5, false, "manual")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}

	pace := lc.Pace("claude")
	if pace == "" {
		t.Fatal("Pace returned empty")
	}
}

func TestLedgerCommands_Pace_Unknown(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	out := lc.Pace("unknown")
	if out == "" {
		t.Fatal("expected output for unknown surface")
	}
}

func TestLedgerCommands_Pace_Exhausted(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 100, WindowDays: nil}
	lc := NewLedgerCommands(l)

	out := lc.Pace("claude")
	if out == "" {
		t.Fatal("Pace exhausted returned empty")
	}
}

func TestLedgerCommands_AccountSet(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)

	out, err := lc.AccountSet("claude", "a@x", 33, 1, "manual")
	if err != nil {
		t.Fatalf("AccountSet: %v", err)
	}
	if out == "" {
		t.Fatal("AccountSet returned empty")
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(rec.Accounts))
	}
	if rec.Accounts[0].Email != "a@x" {
		t.Errorf("expected a@x, got %s", rec.Accounts[0].Email)
	}
}

func TestLedgerCommands_AccountSet_Replace(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "old@x", UsedPct: 50, BurnOrder: 1},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
	}
	lc := NewLedgerCommands(l)

	_, err := lc.AccountSet("claude", "new@y", 10, 2, "replaced")
	if err != nil {
		t.Fatalf("AccountSet replace: %v", err)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(rec.Accounts))
	}
}

func TestLedgerCommands_AccountSet_NoEmail(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	_, err := lc.AccountSet("claude", "", 0, 0, "")
	if err == nil {
		t.Fatal("expected error for empty email")
	}
}

func TestLedgerCommands_AccountExhausted(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 30, BurnOrder: 1},
		},
	}
	lc := NewLedgerCommands(l)

	out, err := lc.AccountExhausted("claude", "a@x", 4, false)
	if err != nil {
		t.Fatalf("AccountExhausted: %v", err)
	}
	if out == "" {
		t.Fatal("expected output")
	}

	// clear
	out, err = lc.AccountExhausted("claude", "a@x", 0, true)
	if err != nil {
		t.Fatalf("AccountExhausted clear: %v", err)
	}
	if out == "" {
		t.Fatal("expected output for clear")
	}
}

func TestLedgerCommands_AccountExhausted_NoEmail(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	_, err := lc.AccountExhausted("claude", "", 0, false)
	if err == nil {
		t.Fatal("expected error for empty email")
	}
}

func TestLedgerCommands_Exhausted(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)

	out, err := lc.Exhausted("claude", 4)
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if out == "" {
		t.Fatal("expected output")
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 100 {
		t.Errorf("expected 100, got %d", rec.UsedPct)
	}
}

func TestLedgerCommands_AccountList_Single(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 42}
	lc := NewLedgerCommands(l)
	out := lc.AccountList("claude")
	if out == "" {
		t.Fatal("expected output")
	}
}

func TestLedgerCommands_AccountList_Multi(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 30, BurnOrder: 1},
			{Email: "b@y", UsedPct: 10, BurnOrder: 2},
		},
	}
	lc := NewLedgerCommands(l)
	out := lc.AccountList("claude")
	if out == "" {
		t.Fatal("expected output")
	}
}

func TestLedgerCommands_AccountList_Missing(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	out := lc.AccountList("missing")
	if out == "" {
		t.Fatal("expected output for missing surface")
	}
}

func TestLedgerCommands_Selftest(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	out := lc.Selftest()
	if out != "herd-credits selftest: PASS" {
		t.Fatalf("selftest failed: %s", out)
	}
}

func TestLedgerCommands_Advise(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 50, WindowDays: intPtr(7), DaysLeft: intPtr(5)}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if out == "" {
		t.Fatal("expected output")
	}
}

func TestCcUsageBlocks_NonClaude(t *testing.T) {
	out := CcUsageBlocks("openai", 10)
	if out != "" {
		t.Errorf("expected empty, got %s", out)
	}
}

func TestCcUsageDaily_NonClaude(t *testing.T) {
	out := CcUsageDaily("openai", "20250101", 10)
	if out != "" {
		t.Errorf("expected empty, got %s", out)
	}
}

func TestRefresh_NoActiveEmail(t *testing.T) {
	dir := t.TempDir()
	os.Unsetenv("HERD_CREDITS_NO_REFRESH")

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
}
