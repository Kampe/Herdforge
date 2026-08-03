package credits

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	if r.Output != "herd-credits: refresh only wired for claude (ccusage has no data for openai)" {
		t.Errorf("exact non-claude skip message mismatch, got: %q", r.Output)
	}
	if _, err := l.Surface("openai"); err == nil {
		t.Error("non-claude skip must not create a ledger record")
	}
}

func TestRefresh_SkipNoRefreshEnv(t *testing.T) {
	t.Setenv("HERD_CREDITS_NO_REFRESH", "1")
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 42, Note: "manual"}
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.Output != "herd-credits: refresh skipped (HERD_CREDITS_NO_REFRESH=1)" {
		t.Errorf("exact NO_REFRESH skip message mismatch, got: %q", r.Output)
	}
	rec, _ := l.Surface("claude")
	if rec.UsedPct != 42 || rec.Note != "manual" {
		t.Errorf("NO_REFRESH skip must leave state unchanged, got %+v", rec)
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

func TestLedgerCommands_Show_SingleRow(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    42,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Note:       "manual",
	}
	lc := NewLedgerCommands(l)
	out := lc.Show()
	want := "claude: 42% used, 5/7d left  [manual]"
	if out != want {
		t.Errorf("exact single Show row mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestLedgerCommands_Show_MultiRow(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 33, BurnOrder: 1},
			{Email: "b@y", UsedPct: 0, BurnOrder: 2},
		},
		Note: "pool note",
	}
	lc := NewLedgerCommands(l)
	out := lc.Show()
	want := "claude: multi N=2 effective=16%  [pool note]"
	if out != want {
		t.Errorf("exact multi Show row mismatch:\n got: %q\nwant: %q", out, want)
	}
}

func TestLedgerCommands_SetAndPace(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)

	out, err := lc.Set("claude", 42, 7, 5, false, "manual")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(out, "claude: 42% used") {
		t.Errorf("Set output must contain pace line with 42%% used, got: %s", out)
	}

	rec, err := l.Surface("claude")
	if err != nil {
		t.Fatalf("expected claude record after Set: %v", err)
	}
	if rec.UsedPct != 42 {
		t.Errorf("expected UsedPct=42, got %d", rec.UsedPct)
	}
	if rec.WindowDays == nil || *rec.WindowDays != 7 {
		t.Errorf("expected WindowDays=7, got %v", rec.WindowDays)
	}
	if rec.DaysLeft == nil || *rec.DaysLeft != 5 {
		t.Errorf("expected DaysLeft=5, got %v", rec.DaysLeft)
	}
	if rec.Note != "manual" {
		t.Errorf("Set must store caller note verbatim, got %q", rec.Note)
	}

	// verify persisted to disk, not just in memory
	l2, err := OpenLedger(l.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec2, _ := l2.Surface("claude")
	if rec2.UsedPct != 42 {
		t.Errorf("Set mutation must persist to disk, got %d after reopen", rec2.UsedPct)
	}
}

func TestLedgerCommands_Set_PreservesTopLevelSource(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    10,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(6),
		Note:       "ccusage",
		Source:     "ccusage",
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)

	if _, err := lc.Set("claude", 77, 7, 5, false, "manual"); err != nil {
		t.Fatalf("Set: %v", err)
	}

	rec, _ := l.Surface("claude")
	if rec.Source != "ccusage" {
		t.Errorf("Set merge must preserve top-level source, got %q", rec.Source)
	}
	if rec.UsedPct != 77 {
		t.Errorf("Set must update used_pct, got %d", rec.UsedPct)
	}
	if rec.Note != "manual" {
		t.Errorf("Set must update note, got %q", rec.Note)
	}

	// and it must persist
	l2, _ := OpenLedger(l.Path())
	rec2, _ := l2.Surface("claude")
	if rec2.Source != "ccusage" {
		t.Errorf("preserved source must persist to disk, got %q after reopen", rec2.Source)
	}
}

func TestLedgerCommands_Set_AuthStatusFallbackUpdatesPoolAccount(t *testing.T) {
	dir := t.TempDir()
	// no sidecar anywhere
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", filepath.Join(dir, "no-accounts"))
	t.Setenv("HOME", filepath.Join(dir, "no-home"))

	orig := claudeAuthStatusCmd
	defer func() { claudeAuthStatusCmd = orig }()
	claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", `echo '{"email":"auth@example.com"}'`)
	}

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    10,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Accounts: []AccountRow{
			{Email: "auth@example.com", UsedPct: 10, BurnOrder: 1},
			{Email: "other@example.com", UsedPct: 20, BurnOrder: 2},
		},
	}
	lc := NewLedgerCommands(l)

	out, err := lc.Set("claude", 77, 7, 5, false, "manual")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !strings.Contains(out, "herd-credits: claude active account auth@example.com used=77%") {
		t.Errorf("Set must print active-account line via auth-status fallback, got: %s", out)
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 77 {
		t.Errorf("top-level used must be 77, got %d", rec.UsedPct)
	}
	for _, a := range rec.Accounts {
		if a.Email == "auth@example.com" && a.UsedPct != 77 {
			t.Errorf("matching pool account must be updated to 77, got %d", a.UsedPct)
		}
		if a.Email == "other@example.com" && a.UsedPct != 20 {
			t.Errorf("non-matching pool account must stay 20, got %d", a.UsedPct)
		}
	}
}

func TestLedgerCommands_Set_NonClaudeSurfaceIgnoresClaudeIdentity(t *testing.T) {
	dir := t.TempDir()
	// claude sidecar present — must NOT be consulted for non-claude surfaces
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("sidecar@example.com\n"), 0644)

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["agy"] = Record{
		UsedPct:    10,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Accounts: []AccountRow{
			{Email: "sidecar@example.com", UsedPct: 10, BurnOrder: 1},
		},
	}
	lc := NewLedgerCommands(l)

	out, err := lc.Set("agy", 66, 7, 5, false, "manual")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if strings.Contains(out, "active account") {
		t.Errorf("non-claude surface must not print claude active-account line, got: %s", out)
	}

	rec, _ := l.Surface("agy")
	for _, a := range rec.Accounts {
		if a.UsedPct != 10 {
			t.Errorf("non-claude surface must not update accounts from claude identity, got %d", a.UsedPct)
		}
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

func TestLedgerCommands_Pace_PausedSurfaceNote(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    20,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Note:       "PAUSED: billing issue",
	}
	lc := NewLedgerCommands(l)

	out := lc.Pace("claude")
	if !strings.Contains(out, "PAUSED") {
		t.Errorf("expected PAUSED in pace output for paused note, got: %s", out)
	}
	if !strings.Contains(out, "exhausted") && !strings.Contains(out, "0") {
		t.Errorf("expected concurrency 0 / exhausted for paused surface, got: %s", out)
	}
}

func TestLedgerCommands_AccountSet(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)

	out, err := lc.AccountSet("claude", "a@x", 33, 1, "manual")
	if err != nil {
		t.Fatalf("AccountSet: %v", err)
	}
	if out != "herd-credits: claude account a@x used=33% burn_order=1" {
		t.Errorf("exact output mismatch, got: %q", out)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(rec.Accounts))
	}
	a := rec.Accounts[0]
	if a.Email != "a@x" || a.UsedPct != 33 || a.BurnOrder != 1 || a.Note != "manual" {
		t.Errorf("account row mismatch: %+v", a)
	}
	// primary UsedPct propagation
	if rec.UsedPct != 33 {
		t.Errorf("top-level UsedPct must propagate from primary, got %d", rec.UsedPct)
	}
	// exact one-decimal generated pool note
	if rec.Note != "multi-account pool N=1 effective=33.0%; manual" {
		t.Errorf("pool note mismatch, got %q", rec.Note)
	}

	// disk persistence
	l2, err := OpenLedger(l.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec2, _ := l2.Surface("claude")
	if len(rec2.Accounts) != 1 || rec2.Accounts[0].UsedPct != 33 {
		t.Errorf("AccountSet must persist to disk, got %+v after reopen", rec2.Accounts)
	}
}

func TestLedgerCommands_AccountSet_CaseInsensitiveReplace(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "A@X", UsedPct: 50, BurnOrder: 1, Note: "old"},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
	}
	lc := NewLedgerCommands(l)

	_, err := lc.AccountSet("claude", "a@x", 10, 2, "new")
	if err != nil {
		t.Fatalf("AccountSet replace: %v", err)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 1 {
		t.Fatalf("case-insensitive replace must keep 1 account, got %d", len(rec.Accounts))
	}
	if rec.Accounts[0].UsedPct != 10 || rec.Accounts[0].BurnOrder != 2 || rec.Accounts[0].Note != "new" {
		t.Errorf("replaced account mismatch: %+v", rec.Accounts[0])
	}
}

func TestLedgerCommands_AccountSet_BurnOrdering(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)

	if _, err := lc.AccountSet("claude", "second@x", 20, 2, ""); err != nil {
		t.Fatalf("AccountSet: %v", err)
	}
	if _, err := lc.AccountSet("claude", "first@x", 40, 1, ""); err != nil {
		t.Fatalf("AccountSet: %v", err)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 2 {
		t.Fatalf("expected 2 accounts, got %d", len(rec.Accounts))
	}
	if rec.Accounts[0].Email != "first@x" || rec.Accounts[1].Email != "second@x" {
		t.Errorf("accounts must be sorted by burn order, got %+v", rec.Accounts)
	}
	// primary (min burn order) UsedPct propagates to top level
	if rec.UsedPct != 40 {
		t.Errorf("top-level UsedPct must be primary 40, got %d", rec.UsedPct)
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

	before := NowEpoch()
	out, err := lc.AccountExhausted("claude", "a@x", 4, false)
	if err != nil {
		t.Fatalf("AccountExhausted: %v", err)
	}
	if !strings.Contains(out, "HOURLY-DEAD") || !strings.Contains(out, "~4h") {
		t.Errorf("expected HOURLY-DEAD ~4h output, got: %s", out)
	}

	rec, _ := l.Surface("claude")
	dead := rec.Accounts[0].ExhaustedUntil
	if dead < before+14300 || dead > before+14500 {
		t.Errorf("ExhaustedUntil should be ~4h from now (%d), got %d", before+14400, dead)
	}

	out, err = lc.AccountExhausted("claude", "a@x", 0, true)
	if err != nil {
		t.Fatalf("AccountExhausted clear: %v", err)
	}
	if !strings.Contains(out, "cleared") {
		t.Errorf("expected cleared output, got: %s", out)
	}

	rec, _ = l.Surface("claude")
	if rec.Accounts[0].ExhaustedUntil != 0 {
		t.Errorf("clear must reset ExhaustedUntil to 0, got %d", rec.Accounts[0].ExhaustedUntil)
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
	l.data["claude"] = Record{
		UsedPct:    40,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 40, BurnOrder: 1},
			{Email: "b@y", UsedPct: 10, BurnOrder: 2},
		},
	}
	lc := NewLedgerCommands(l)

	out, err := lc.Exhausted("claude", 4)
	if err != nil {
		t.Fatalf("Exhausted: %v", err)
	}
	if out != "herd-credits: claude marked exhausted (~4h). Also set a herd-route cooldown for the hard gate." {
		t.Errorf("exact output mismatch, got: %q", out)
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 100 {
		t.Errorf("expected UsedPct=100, got %d", rec.UsedPct)
	}
	if rec.WindowDays != nil {
		t.Errorf("window_days must be null after exhaustion, got %v", *rec.WindowDays)
	}
	if rec.DaysLeft != nil {
		t.Errorf("days_left must be null after exhaustion, got %v", *rec.DaysLeft)
	}
	if rec.Note != "exhausted, resets in ~4h" {
		t.Errorf("exact exhaustion note mismatch, got %q", rec.Note)
	}
	// account propagation
	for _, a := range rec.Accounts {
		if a.UsedPct != 100 {
			t.Errorf("all accounts must propagate to 100, got %+v", a)
		}
	}

	// disk persistence
	l2, err := OpenLedger(l.Path())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	rec2, _ := l2.Surface("claude")
	if rec2.UsedPct != 100 || rec2.WindowDays != nil {
		t.Errorf("Exhausted must persist to disk, got %+v after reopen", rec2)
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
			{Email: "b@y", UsedPct: 0, BurnOrder: 2},
			{Email: "a@x", UsedPct: 33, BurnOrder: 1},
		},
	}
	lc := NewLedgerCommands(l)
	out := lc.AccountList("claude")
	// actual average of (33,0) = 16.5 — must not be rounded to an integer
	if !strings.Contains(out, "claude: multi-account N=2 effective=16.5%") {
		t.Errorf("expected effective=16.5%% header, got: %s", out)
	}
	// burn-order sort: a@x (burn#1) must appear before b@y (burn#2)
	idxA := strings.Index(out, "a@x")
	idxB := strings.Index(out, "b@y")
	if idxA < 0 || idxB < 0 || idxA > idxB {
		t.Errorf("accounts must be sorted by burn order, got: %s", out)
	}
	if !strings.Contains(out, "burn#1") || !strings.Contains(out, "burn#2") {
		t.Errorf("expected burn-order labels, got: %s", out)
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

func TestLedgerCommands_Advise_LiveUnavailable_ShowsFallback(t *testing.T) {
	// ensure bin/herd-quota does not exist so live quota is unavailable
	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return "/nonexistent/herd-quota" }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    50,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if !strings.Contains(out, "live quota unavailable") {
		t.Errorf("expected live-unavailable header, got: %s", out)
	}
	if !strings.Contains(out, "claude:") {
		t.Errorf("expected ledger pace rows for claude, got: %s", out)
	}
}

func TestLedgerCommands_Advise_LiveCoversProvider(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"used":42.7,"pools":{"claude-5h":{"used":42.7,"remaining":57.3,"resetsIn":"3h 0m","class":"onpace","stale":false,"reason":"ok","exhaustsBeforeReset":false},"all":{"used":42,"stale":false,"reason":"ok"}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    50,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if !strings.Contains(out, "live OpenUsage quota") {
		t.Errorf("expected live OpenUsage header, got: %s", out)
	}
	if !strings.Contains(out, "  claude/claude-5h: 42% used, 57% left, onpace, reset 3h 0m, safe through reset at current burn\n") {
		t.Errorf("expected full binding row incl. safe-through-reset suffix, got: %s", out)
	}
	if !strings.Contains(out, "ledger-only fallback snapshots") {
		t.Errorf("expected ledger-only fallback header on live success, got: %s", out)
	}
	// claude should NOT get a ledger pace row because live covers it (by provider)
	if strings.Contains(out, "claude: 50% used") {
		t.Errorf("ledger pace row for claude should be suppressed when live covers provider, got: %s", out)
	}
}

func TestLedgerCommands_Advise_RunwayBranches(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"pools":{"claude-5h":{"used":96,"remaining":4,"resetsIn":"2h","class":"exhausted","stale":false,"reason":"exhausted","exhaustsBeforeReset":true,"runwayMinutes":30}}},"antigravity":{"pools":{"agy":{"used":80,"remaining":20,"resetsIn":"1d","class":"overpace","stale":false,"reason":"ok","exhaustsBeforeReset":true,"runwayMinutes":150}}},"openai":{"pools":{"codex":{"used":50,"remaining":50,"resetsIn":"5h","class":"onpace","stale":false,"reason":"ok"}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if !strings.Contains(out, "  claude/claude-5h: 96% used, 4% left, exhausted, reset 2h, exhausted until reset\n") {
		t.Errorf("expected exhausted-until-reset branch, got: %s", out)
	}
	if !strings.Contains(out, "  antigravity/agy: 80% used, 20% left, overpace, reset 1d, projected runway 2h\n") {
		t.Errorf("expected projected-runway branch (150min -> 2h), got: %s", out)
	}
	if !strings.Contains(out, "  openai/codex: 50% used, 50% left, onpace, reset 5h, exhaustion runway unknown\n") {
		t.Errorf("expected unknown-runway branch (exhaustsBeforeReset absent), got: %s", out)
	}
}

func TestLedgerCommands_Advise_MalformedUsedDoesNotSuppressFallback(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"pools":{"claude-5h":{"used":"bad","remaining":57.3,"resetsIn":"3h","class":"onpace","stale":false,"reason":"ok"}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    50,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	// malformed used must not render a live row
	if strings.Contains(out, "claude/claude-5h:") {
		t.Errorf("malformed used must not render a live row, got: %s", out)
	}
	if strings.Contains(out, "0% used, 100% left") {
		t.Errorf("malformed used must not render 0%%/100%%, got: %s", out)
	}
	// and must not suppress the fail-closed ledger fallback
	if !strings.Contains(out, "claude: 50% used") {
		t.Errorf("ledger fallback must print when live data is invalid, got: %s", out)
	}
}

func TestLedgerCommands_Advise_MissingRemainingDoesNotSuppressFallback(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"pools":{"claude-5h":{"used":42.7,"resetsIn":"3h","class":"onpace","stale":false,"reason":"ok"}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    50,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if strings.Contains(out, "claude/claude-5h:") {
		t.Errorf("missing remaining must not render a live row, got: %s", out)
	}
	if strings.Contains(out, "0% left") {
		t.Errorf("missing remaining must not fabricate 0%% left, got: %s", out)
	}
	if !strings.Contains(out, "claude: 50% used") {
		t.Errorf("ledger fallback must print when remaining is missing, got: %s", out)
	}
}

func TestLedgerCommands_Advise_LiveMalformedJSON(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo 'not json'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    30,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if !strings.Contains(out, "live quota unavailable") {
		t.Errorf("expected live-unavailable header on malformed JSON, got: %s", out)
	}
	if !strings.Contains(out, "claude:") {
		t.Errorf("expected ledger pace rows on malformed JSON, got: %s", out)
	}
}

func TestLedgerCommands_Advise_LiveFiltersStalePools(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"pools":{"claude-5h":{"used":42,"remaining":58,"stale":true,"reason":"ok"}}},"antigravity":{"pools":{"agy":{"used":10,"remaining":90,"resetsIn":"5d","class":"onpace","stale":false,"reason":"exhausted"}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 20, WindowDays: intPtr(7), DaysLeft: intPtr(5), Updated: NowEpoch()}
	l.data["agy"] = Record{UsedPct: 10, WindowDays: intPtr(7), DaysLeft: intPtr(5), Updated: NowEpoch()}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	// stale pool should be skipped from live rows
	if strings.Contains(out, "claude/claude-5h:") {
		t.Errorf("stale pool should not appear in live rows, got: %s", out)
	}
	// antigravity has a valid live pool (reason=exhausted, stale=false) -> live row shown
	if !strings.Contains(out, "  antigravity/agy: 10% used, 90% left, onpace, reset 5d, exhaustion runway unknown\n") {
		t.Errorf("expected full binding row for antigravity/agy, got: %s", out)
	}
	// agy ledger surface normalizes to antigravity, which has live coverage -> suppressed
	if strings.Contains(out, "agy: 10% used, 28% of window") {
		t.Errorf("agy ledger pace should be suppressed when live covers antigravity provider, got: %s", out)
	}
	// claude has no valid live pool (only stale) -> ledger pace still shown
	if !strings.Contains(out, "claude: 20% used") {
		t.Errorf("claude ledger pace should appear when its only pool is stale, got: %s", out)
	}
}

func TestNormalizeProviderKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"claude-5h", "claude"},
		{"claude", "claude"},
		{"agy", "antigravity"},
		{"agy", "antigravity"},
		{"openai", "openai"},
		{"CLAUDE-5H", "claude"},
	}
	for _, tt := range tests {
		got := normalizeProviderKey(tt.in)
		if got != tt.want {
			t.Errorf("normalizeProviderKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
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

func TestParseTokensFromJSON_Real(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	jsonStr := `[{"totalTokens": 42000}]`
	got := ParseTokensFromJSON(jsonStr, ".[0].totalTokens")
	if got != 42000 {
		t.Errorf("expected 42000, got %d", got)
	}
}

func TestParseTokensFromJSON_EmptyInput(t *testing.T) {
	got := ParseTokensFromJSON("", ".totalTokens")
	if got != 0 {
		t.Errorf("expected 0 for empty input, got %d", got)
	}
}

func TestParseTokensFromJSON_NegativeClamped(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	jsonStr := `[{"totalTokens": -5}]`
	got := ParseTokensFromJSON(jsonStr, ".[0].totalTokens")
	if got != 0 {
		t.Errorf("expected negative clamped to 0, got %d", got)
	}
}

func TestParseRemainingMinutes_Real(t *testing.T) {
	jsonStr := `{"blocks": [{"isActive": true, "projection": {"remainingMinutes": 240}}]}`
	got := ParseRemainingMinutes(jsonStr)
	if got != 240 {
		t.Errorf("expected 240, got %d", got)
	}
}

func TestParseRemainingMinutes_MalformedJSON(t *testing.T) {
	got := ParseRemainingMinutes("not json")
	if got != 0 {
		t.Errorf("expected 0 for malformed JSON, got %d", got)
	}
}

func TestParseRemainingMinutes_NoActiveBlock(t *testing.T) {
	jsonStr := `{"blocks": [{"isActive": false, "projection": {"remainingMinutes": 240}}]}`
	got := ParseRemainingMinutes(jsonStr)
	if got != 0 {
		t.Errorf("expected 0 when no active block, got %d", got)
	}
}

func TestParseRemainingMinutes_EmptyInput(t *testing.T) {
	got := ParseRemainingMinutes("")
	if got != 0 {
		t.Errorf("expected 0 for empty input, got %d", got)
	}
}

func TestExecCcUsage_Timeout(t *testing.T) {
	orig := CcUsageBase
	defer func() { CcUsageBase = orig }()

	CcUsageBase = func() (string, error) {
		return "sleep", nil
	}

	out := execCcUsage([]string{"99"}, 1)
	if out != "" {
		t.Errorf("expected empty on timeout, got %q", out)
	}
}

func TestExecCcUsage_Error(t *testing.T) {
	orig := CcUsageBase
	defer func() { CcUsageBase = orig }()

	CcUsageBase = func() (string, error) {
		return "false", nil
	}

	out := execCcUsage([]string{""}, 10)
	if out != "" {
		t.Errorf("expected empty on error exit, got %q", out)
	}
}

func TestClaudeActiveExpanded_JSONPath(t *testing.T) {
	orig := claudeAuthStatusCmd
	defer func() { claudeAuthStatusCmd = orig }()

	claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
		script := `echo '{"email":"test@example.com"}'`
		return exec.Command("sh", "-c", script)
	}

	email := ClaudeActiveExpanded()
	if email != "test@example.com" {
		t.Errorf("expected test@example.com, got %q", email)
	}
}

func TestClaudeActiveExpanded_TextFallback(t *testing.T) {
	orig := claudeAuthStatusCmd
	defer func() { claudeAuthStatusCmd = orig }()

	claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
		script := `echo "not json" && echo "Email:  alt@example.com  "`
		return exec.Command("sh", "-c", script)
	}

	email := ClaudeActiveExpanded()
	if email != "alt@example.com" {
		t.Errorf("expected alt@example.com, got %q", email)
	}
}

func TestClaudeActiveExpanded_Error(t *testing.T) {
	orig := claudeAuthStatusCmd
	defer func() { claudeAuthStatusCmd = orig }()

	claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "exit 1")
	}

	email := ClaudeActiveExpanded()
	if email != "" {
		t.Errorf("expected empty on error, got %q", email)
	}
}

func TestClaudeActiveExpanded_TextEmailTrimmed(t *testing.T) {
	orig := claudeAuthStatusCmd
	defer func() { claudeAuthStatusCmd = orig }()

	claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
		script := `echo "Email:  spaced@example.com"`
		return exec.Command("sh", "-c", script)
	}

	email := ClaudeActiveExpanded()
	if email != "spaced@example.com" {
		t.Errorf("expected trimmed email, got %q", email)
	}
}

func TestRefresh_NoActiveEmail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	// pin identity lookups to empty locations
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", filepath.Join(dir, "no-accounts"))
	t.Setenv("HOME", filepath.Join(dir, "no-home"))

	// auth status fails too — no identity source exists
	origCmd := claudeAuthStatusCmd
	defer func() { claudeAuthStatusCmd = origCmd }()
	claudeAuthStatusCmd = func(args ...string) *exec.Cmd {
		return exec.Command("sh", "-c", "exit 1")
	}

	// ccusage must never be reached: fail loudly if it is
	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) {
		return "", fmt.Errorf("ccusage must not be invoked without an active account")
	}

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{UsedPct: 42, Note: "manual", Updated: NowEpoch()}

	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if r.Output != "herd-credits: refresh skipped (no active claude account known)" {
		t.Errorf("exact no-active line mismatch, got: %q", r.Output)
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 42 || rec.Note != "manual" {
		t.Errorf("ledger must be unchanged on no-active skip, got %+v", rec)
	}
}

// --- non-vacuous tests for findings ---

func TestLedgerCommands_Pace_AllExhaustedPool(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 98, BurnOrder: 1},
			{Email: "b@y", UsedPct: 96, BurnOrder: 2},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
	}
	lc := NewLedgerCommands(l)
	out := lc.Pace("claude")
	// all accounts >=95 → should classify as exhausted, concurrency 0
	if !strings.Contains(out, "exhausted") {
		t.Errorf("all-exhausted pool should classify as exhausted, got: %s", out)
	}
	if !strings.Contains(out, "concurrency 0") {
		t.Errorf("all-exhausted pool should have concurrency 0, got: %s", out)
	}
}

func TestLedgerCommands_Pace_PartiallyExhaustedPool(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 98, BurnOrder: 1},
			{Email: "b@y", UsedPct: 50, BurnOrder: 2},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
	}
	lc := NewLedgerCommands(l)
	out := lc.Pace("claude")
	// primary exhausted, reserve has headroom → should show SWITCH_AUTH
	if !strings.Contains(out, "SWITCH_AUTH") {
		t.Errorf("partially exhausted pool should show SWITCH_AUTH, got: %s", out)
	}
}

func TestLedgerCommands_Pace_PrimaryIsMinBurnOrder(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 10, BurnOrder: 3},
			{Email: "b@y", UsedPct: 96, BurnOrder: 1},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
	}
	lc := NewLedgerCommands(l)
	out := lc.Pace("claude")
	// b@y has burn order 1 (minimum), so it's the primary; it's exhausted
	// a@x has headroom, so reserve = a@x
	if !strings.Contains(out, "SWITCH_AUTH") {
		t.Errorf("primary=b@y exhausted should show SWITCH_AUTH, got: %s", out)
	}
	if !strings.Contains(out, "claude-account use") {
		t.Errorf("SWITCH_AUTH should include claude-account action, got: %s", out)
	}
}

func TestLedgerCommands_Pace_SurfaceNoteDoNotDispatch(t *testing.T) {
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    20,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Note:       "do-not-dispatch",
	}
	lc := NewLedgerCommands(l)
	out := lc.Pace("claude")
	if !strings.Contains(out, "PAUSED") {
		t.Errorf("do-not-dispatch note should trigger PAUSED, got: %s", out)
	}
	if !strings.Contains(out, "concurrency 0") {
		t.Errorf("paused surface should have concurrency 0, got: %s", out)
	}
}

func TestParseTokensFromJSON_SumsMultipleValues(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	jsonStr := `{"blocks":[{"isActive":true,"totalTokens":1000},{"isActive":true,"totalTokens":2000},{"isActive":false,"totalTokens":5000}]}`
	got := ParseTokensFromJSON(jsonStr, ".blocks[] | select(.isActive == true) | .totalTokens")
	if got != 3000 {
		t.Errorf("expected 3000 (1000+2000), got %d", got)
	}
}

func TestParseQuotaInt(t *testing.T) {
	tests := []struct {
		in   interface{}
		want int
		ok   bool
	}{
		{42.0, 42, true},
		{"42", 42, true},
		{"42.7", 42, true},
		{42, 42, true},
		{nil, 0, false},
		{"not a number", 0, false},
		{"NaN", 0, false},
		{"Inf", 0, false},
		{"+Inf", 0, false},
		{"-Inf", 0, false},
		{math.NaN(), 0, false},
		{math.Inf(1), 0, false},
		{math.Inf(-1), 0, false},
		{"9e300", 0, false},
		{1e300, 0, false},
	}
	for _, tt := range tests {
		got, ok := parseQuotaInt(tt.in)
		if ok != tt.ok {
			t.Errorf("parseQuotaInt(%v) ok = %v, want %v", tt.in, ok, tt.ok)
		}
		if ok && got != tt.want {
			t.Errorf("parseQuotaInt(%v) = %d, want %d", tt.in, got, tt.want)
		}
	}
}

func TestRefresh_HappyPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("test@example.com\n"), 0644)

	// mock ccusage to return valid JSON
	mockScript := filepath.Join(dir, "ccusage")
	blocksJSON := `{"blocks":[{"isActive":true,"totalTokens":10000000,"projection":{"remainingMinutes":120}}]}`
	dailyJSON := `{"totals":{"totalTokens":50000000}}`
	os.WriteFile(mockScript, []byte(fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "blocks"; then
  echo '%s'
else
  echo '%s'
fi`, blocksJSON, dailyJSON)), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("Refresh: %v", r.Err)
	}
	if r.Output == "" {
		t.Fatal("expected output from happy-path refresh")
	}

	rec, err := l.Surface("claude")
	if err != nil {
		t.Fatalf("expected claude record after refresh, got: %v", err)
	}
	if rec.UsedPct != 50 {
		t.Errorf("expected 50%% (50M/100M weekly), got %d%%", rec.UsedPct)
	}
}

func TestRefresh_PreservesOperatorNotes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("a@x\n"), 0644)

	mockScript := filepath.Join(dir, "ccusage")
	blocksJSON := `{"blocks":[{"isActive":true,"totalTokens":1000000,"projection":{"remainingMinutes":120}}]}`
	dailyJSON := `{"totals":{"totalTokens":50000000}}`
	os.WriteFile(mockScript, []byte(fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "blocks"; then
  echo '%s'
else
  echo '%s'
fi`, blocksJSON, dailyJSON)), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{
		UsedPct:    10,
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Note:       "PAUSED do-not-dispatch",
		Updated:    NowEpoch(),
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 10, BurnOrder: 1, Note: "operator parked"},
		},
	}

	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("Refresh: %v", r.Err)
	}

	rec, _ := l.Surface("claude")
	if rec.Note != "PAUSED do-not-dispatch" {
		t.Errorf("surface note must be preserved through refresh, got %q", rec.Note)
	}
	if len(rec.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(rec.Accounts))
	}
	if rec.Accounts[0].Note != "operator parked" {
		t.Errorf("account note must be preserved through refresh, got %q", rec.Accounts[0].Note)
	}
	if rec.Accounts[0].UsedPct != 50 {
		t.Errorf("account used should be updated to 50%%, got %d%%", rec.Accounts[0].UsedPct)
	}
	if rec.Accounts[0].Source != "ccusage" {
		t.Errorf("refresh merge must write source=ccusage, got %q", rec.Accounts[0].Source)
	}
}

func TestRefresh_WrongShapeJSONLeavesSnapshot(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("test@example.com\n"), 0644)

	// valid JSON but wrong shape: {} for both calls
	mockScript := filepath.Join(dir, "ccusage")
	os.WriteFile(mockScript, []byte(`#!/bin/sh
echo '{}'`), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{UsedPct: 42, Note: "manual", Updated: NowEpoch()}

	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if !strings.Contains(r.Output, "malformed") {
		t.Errorf("expected malformed notice for wrong-shaped JSON, got: %s", r.Output)
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 42 {
		t.Errorf("wrong-shaped JSON must leave snapshot untouched, got used=%d%%", rec.UsedPct)
	}
	if rec.Note != "manual" {
		t.Errorf("wrong-shaped JSON must leave note untouched, got %q", rec.Note)
	}
	if rec.Source == "ccusage" {
		t.Errorf("wrong-shaped JSON must not set source=ccusage")
	}
}

func TestRefresh_WeeklyClamp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("test@example.com\n"), 0644)

	mockScript := filepath.Join(dir, "ccusage")
	blocksJSON := `{"blocks":[{"isActive":true,"totalTokens":1000000,"projection":{"remainingMinutes":120}}]}`
	dailyJSON := `{"totals":{"totalTokens":250000000}}` // 250% of 100M budget
	os.WriteFile(mockScript, []byte(fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "blocks"; then
  echo '%s'
else
  echo '%s'
fi`, blocksJSON, dailyJSON)), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("Refresh: %v", r.Err)
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 100 {
		t.Errorf("weekly pct must clamp at 100, got %d%%", rec.UsedPct)
	}
}

func TestRefresh_HourlyDeadTiming(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("a@x\n"), 0644)

	mockScript := filepath.Join(dir, "ccusage")
	// 19M of 20M 5h budget = 95% -> hourly dead; remainingMinutes=120 -> hold 2h
	blocksJSON := `{"blocks":[{"isActive":true,"totalTokens":19000000,"projection":{"remainingMinutes":120}}]}`
	dailyJSON := `{"totals":{"totalTokens":10000000}}`
	os.WriteFile(mockScript, []byte(fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "blocks"; then
  echo '%s'
else
  echo '%s'
fi`, blocksJSON, dailyJSON)), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{{Email: "a@x", UsedPct: 10, BurnOrder: 1}},
	}

	before := NowEpoch()
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("Refresh: %v", r.Err)
	}
	if !strings.Contains(r.Output, "hourly-dead") {
		t.Errorf("expected hourly-dead in output, got: %s", r.Output)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(rec.Accounts))
	}
	dead := rec.Accounts[0].ExhaustedUntil
	if dead < before+7100 || dead > before+7300 {
		t.Errorf("ExhaustedUntil should be ~2h from now (%d), got %d", before+7200, dead)
	}
}

func TestMaybeRefresh_StaleAndFresh(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_AUTO_REFRESH", "1")
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CREDITS_REFRESH_TTL", "60")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("test@example.com\n"), 0644)

	mockScript := filepath.Join(dir, "ccusage")
	blocksJSON := `{"blocks":[{"isActive":true,"totalTokens":1000000,"projection":{"remainingMinutes":120}}]}`
	dailyJSON := `{"totals":{"totalTokens":50000000}}`
	os.WriteFile(mockScript, []byte(fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "blocks"; then
  echo '%s'
else
  echo '%s'
fi`, blocksJSON, dailyJSON)), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	// fresh record (Updated = now) -> no refresh
	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{UsedPct: 42, Note: "manual", Updated: NowEpoch()}
	r := MaybeRefresh(l)
	if r != nil {
		t.Errorf("fresh record must not trigger refresh, got: %+v", r)
	}
	rec, _ := l.Surface("claude")
	if rec.UsedPct != 42 {
		t.Errorf("fresh record must remain untouched, got %d%%", rec.UsedPct)
	}

	// stale record (Updated = 1h ago) -> refresh fires
	l2, _ := OpenLedger(filepath.Join(dir, "ledger2.json"))
	l2.data["claude"] = Record{UsedPct: 42, Note: "manual", Updated: NowEpoch() - 3600}
	r2 := MaybeRefresh(l2)
	if r2 == nil {
		t.Fatal("stale record must trigger refresh")
	}
	rec2, _ := l2.Surface("claude")
	if rec2.UsedPct != 50 {
		t.Errorf("stale record should refresh to 50%%, got %d%%", rec2.UsedPct)
	}
}

func TestRefresh_MalformedJSONLeavesManualSnapshot(t *testing.T) {	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("test@example.com\n"), 0644)

	mockScript := filepath.Join(dir, "ccusage")
	os.WriteFile(mockScript, []byte(`#!/bin/sh
echo 'not json'`), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	l.data["claude"] = Record{UsedPct: 42, Note: "manual", Updated: NowEpoch()}

	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
	if !strings.Contains(r.Output, "malformed") {
		t.Errorf("expected malformed JSON notice, got: %s", r.Output)
	}

	rec, _ := l.Surface("claude")
	if rec.UsedPct != 42 {
		t.Errorf("manual snapshot should be untouched on malformed JSON, got %d%%", rec.UsedPct)
	}
	if rec.Note != "manual" {
		t.Errorf("manual note should be preserved, got %q", rec.Note)
	}
}

func TestRefresh_EmptyAccountsArrayAppendsActive(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CREDITS_NO_REFRESH", "")
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("test@example.com\n"), 0644)

	mockScript := filepath.Join(dir, "ccusage")
	blocksJSON := `{"blocks":[{"isActive":true,"totalTokens":1000000,"projection":{"remainingMinutes":120}}]}`
	dailyJSON := `{"totals":{"totalTokens":50000000}}`
	os.WriteFile(mockScript, []byte(fmt.Sprintf(`#!/bin/sh
if echo "$*" | grep -q "blocks"; then
  echo '%s'
else
  echo '%s'
fi`, blocksJSON, dailyJSON)), 0755)

	origBase := CcUsageBase
	defer func() { CcUsageBase = origBase }()
	CcUsageBase = func() (string, error) { return mockScript, nil }

	// shell-shaped ledger with an explicitly configured EMPTY pool
	ledgerPath := filepath.Join(dir, "ledger.json")
	os.WriteFile(ledgerPath, []byte(`{"claude":{"used_pct":10,"window_days":7,"days_left":5,"note":"pool","updated":1,"accounts":[]}}`), 0644)
	l, err := OpenLedger(ledgerPath)
	if err != nil {
		t.Fatalf("OpenLedger: %v", err)
	}

	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("Refresh: %v", r.Err)
	}

	rec, _ := l.Surface("claude")
	if len(rec.Accounts) != 1 {
		t.Fatalf("empty pool must gain the active account, got %d accounts", len(rec.Accounts))
	}
	a := rec.Accounts[0]
	if a.Email != "test@example.com" || a.UsedPct != 50 || a.BurnOrder != 99 || a.Source != "ccusage" {
		t.Errorf("appended account mismatch: %+v", a)
	}
	if rec.UsedPct != 50 {
		t.Errorf("appended account must become primary (used=50), got %d", rec.UsedPct)
	}
	if rec.Source == "ccusage" {
		t.Errorf("accounts branch must not write top-level source, got %q", rec.Source)
	}
}

func TestAdvise_ReadySwap_FalsePositiveGuard(t *testing.T) {
	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return "/nonexistent/herd-quota" }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct: 10,
		Accounts: []AccountRow{
			{Email: "primary@x", UsedPct: 10, BurnOrder: 1},
			{Email: "reserve@y", UsedPct: 100, BurnOrder: 2},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if strings.Contains(out, "READY SWAP") {
		t.Errorf("exhausted burn#2 with healthy burn#1 must NOT print READY SWAP, got: %s", out)
	}
}

func TestAdvise_ReadySwap_TruePositive(t *testing.T) {
	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return "/nonexistent/herd-quota" }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		UsedPct: 96,
		Accounts: []AccountRow{
			{Email: "blindside328@gmail.com", UsedPct: 96, BurnOrder: 1},
			{Email: "nick.kampe@yugalabs.io", UsedPct: 10, BurnOrder: 2},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(5),
		Updated:    NowEpoch(),
	}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if !strings.Contains(out, "READY SWAP (primary exhausted): claude-account use yuga") {
		t.Errorf("exhausted burn#1 must print READY SWAP with reserve account, got: %s", out)
	}
}

func TestLedgerCommands_Advise_NaNUsedDoesNotSuppressFallback(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"pools":{"claude-5h":{"used":"NaN","remaining":57.3,"resetsIn":"3h","class":"onpace","stale":false,"reason":"ok"}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 50, WindowDays: intPtr(7), DaysLeft: intPtr(5), Updated: NowEpoch()}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if strings.Contains(out, "claude/claude-5h:") {
		t.Errorf("NaN used must not render a live row, got: %s", out)
	}
	if !strings.Contains(out, "claude: 50% used") {
		t.Errorf("NaN used must not suppress the ledger fallback, got: %s", out)
	}
}

func TestLedgerCommands_Advise_TrueRunwayWithoutMinutesFallsThrough(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "herd-quota")
	os.WriteFile(script, []byte(`#!/bin/sh
echo '{"claude":{"pools":{"claude-5h":{"used":80,"remaining":20,"resetsIn":"3h","class":"overpace","stale":false,"reason":"ok","exhaustsBeforeReset":true}}}}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 50, WindowDays: intPtr(7), DaysLeft: intPtr(5), Updated: NowEpoch()}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	if strings.Contains(out, "claude/claude-5h:") {
		t.Errorf("exhaustsBeforeReset:true with absent runwayMinutes must not render, got: %s", out)
	}
	if strings.Contains(out, "projected runway 0h") {
		t.Errorf("must not fabricate projected runway 0h, got: %s", out)
	}
	if !strings.Contains(out, "claude: 50% used") {
		t.Errorf("malformed runway row must fall through to ledger, got: %s", out)
	}
}

func TestCcUsageBase_PATHCcusageWins(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ccusage"), []byte("#!/bin/sh\n"), 0755)
	os.WriteFile(filepath.Join(dir, "npx"), []byte("#!/bin/sh\n"), 0755)
	t.Setenv("PATH", dir)

	base, err := CcUsageBase()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if base != "ccusage" {
		t.Errorf("installed ccusage must win over npx, got %q", base)
	}
}

func TestCcUsageBase_NpxFallback(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "npx"), []byte("#!/bin/sh\n"), 0755)
	t.Setenv("PATH", dir)

	base, err := CcUsageBase()
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if base != "npx" {
		t.Errorf("npx must be selected when it is the only option, got %q", base)
	}
}

func TestCcUsageBase_NeitherErrors(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PATH", dir)

	_, err := CcUsageBase()
	if err == nil {
		t.Fatal("expected error when neither ccusage nor npx is on PATH")
	}
}

func TestCcUsageArgs_NpxPrefix(t *testing.T) {
	got := ccUsageArgs("npx", []string{"claude", "blocks", "--json"})
	want := []string{"--yes", "ccusage@latest", "claude", "blocks", "--json"}
	if len(got) != len(want) {
		t.Fatalf("ccUsageArgs len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ccUsageArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	got = ccUsageArgs("ccusage", []string{"claude", "blocks", "--json"})
	want = []string{"claude", "blocks", "--json"}
	if len(got) != len(want) {
		t.Fatalf("ccUsageArgs len = %d, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ccUsageArgs[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestEnvInt(t *testing.T) {
	os.Setenv("TEST_ENV_INT", "42")
	defer os.Unsetenv("TEST_ENV_INT")
	got := envInt("TEST_ENV_INT", 10)
	if got != 42 {
		t.Errorf("expected 42, got %d", got)
	}

	os.Setenv("TEST_ENV_INT", "not a number")
	got = envInt("TEST_ENV_INT", 10)
	if got != 10 {
		t.Errorf("expected fallback 10, got %d", got)
	}

	os.Setenv("TEST_ENV_INT", "-5")
	got = envInt("TEST_ENV_INT", 10)
	if got != 10 {
		t.Errorf("expected fallback 10 for negative, got %d", got)
	}
}

func TestLedgerCommands_Pace_FloorTruncation(t *testing.T) {
	// (33,0) pool: avg 16.5 -> floor 16 (not round 17)
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 33, BurnOrder: 1},
			{Email: "b@y", UsedPct: 0, BurnOrder: 2},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(6),
	}
	lc := NewLedgerCommands(l)
	out := lc.Pace("claude")
	if !strings.Contains(out, "effective 16%") {
		t.Errorf("expected floor(16.5)=16, got: %s", out)
	}
}

func TestLedgerCommands_Pace_BoundaryClassifiesOnpace(t *testing.T) {
	// (100,19) pool: avg 59.5 -> floor 59 -> at floor 60, 59 < 60 -> onpace/concurrency 2
	// (round(59.5)=60 would be overpace/concurrency 1)
	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{
		Accounts: []AccountRow{
			{Email: "a@x", UsedPct: 100, BurnOrder: 1},
			{Email: "b@y", UsedPct: 19, BurnOrder: 2},
		},
		WindowDays: intPtr(7),
		DaysLeft:   intPtr(4), // elapsed ~43% > 59% used
	}
	lc := NewLedgerCommands(l)
	out := lc.Pace("claude")
	if !strings.Contains(out, "effective 59%") {
		t.Errorf("expected floor(59.5)=59, got: %s", out)
	}
	if !strings.Contains(out, "onpace") || !strings.Contains(out, "concurrency 2") {
		t.Errorf("59%% at floor 60 must classify onpace/concurrency 2, got: %s", out)
	}
}

func TestOpenLedger_RejectsZeroByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	os.WriteFile(path, []byte(""), 0644)
	_, err := OpenLedger(path)
	if err == nil {
		t.Fatal("zero-byte ledger must be rejected")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected empty-file error, got: %v", err)
	}
}

func TestOpenLedger_RejectsNull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.json")
	os.WriteFile(path, []byte("null"), 0644)
	_, err := OpenLedger(path)
	if err == nil {
		t.Fatal("JSON null ledger must be rejected")
	}
	if !strings.Contains(err.Error(), "null") {
		t.Errorf("expected null error, got: %v", err)
	}
}

func TestClaudeActiveEmail_EnvOverride(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("override@example.com\n"), 0644)

	email := ClaudeActiveEmail("")
	if email != "override@example.com" {
		t.Errorf("expected HERD_CLAUDE_ACCOUNTS_DIR sidecar, got %q", email)
	}
}

func TestClaudeActiveEmail_FirstLineOnly(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	os.WriteFile(filepath.Join(dir, "active.email"), []byte("first@example.com\nstale@example.com\n"), 0644)

	email := ClaudeActiveEmail("")
	if email != "first@example.com" {
		t.Errorf("sidecar must read only the first line (head -1), got %q", email)
	}
}

func TestClaudeActiveEmail_ExplicitDirWins(t *testing.T) {
	envDir := t.TempDir()
	t.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", envDir)
	os.WriteFile(filepath.Join(envDir, "active.email"), []byte("env@example.com\n"), 0644)

	explicitDir := t.TempDir()
	os.WriteFile(filepath.Join(explicitDir, "active.email"), []byte("explicit@example.com\n"), 0644)

	email := ClaudeActiveEmail(explicitDir)
	if email != "explicit@example.com" {
		t.Errorf("explicit accountsDir must win over env, got %q", email)
	}
}

func TestClaudeEmailToAccount_Lowercase(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Blindside328@gmail.com", "blindside328"},
		{"NICK.KAMPE@yugalabs.io", "yuga"},
		{"SomeUser@example.com", "someuser"},
		{"NoAtSign", "noatsign"},
	}
	for _, tt := range tests {
		got := ClaudeEmailToAccount(tt.in)
		if got != tt.want {
			t.Errorf("ClaudeEmailToAccount(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
