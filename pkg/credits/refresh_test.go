package credits

import (
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
echo '{"pools":[{"key":"claude-5h","used":42,"remaining":58000,"resetsIn":180,"class":"onpace","stale":false,"reason":"ok"},{"key":"all","used":42,"stale":false,"reason":"ok"}]}'`), 0755)

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
	if !strings.Contains(out, "claude-5h:") {
		t.Errorf("expected live pool row for claude-5h, got: %s", out)
	}
	if !strings.Contains(out, "42% used") {
		t.Errorf("expected used=42 in live row, got: %s", out)
	}
	if !strings.Contains(out, "58000 remaining") {
		t.Errorf("expected remaining=58000, got: %s", out)
	}
	if !strings.Contains(out, "onpace class") {
		t.Errorf("expected onpace class, got: %s", out)
	}
	// claude should NOT get a ledger pace row because live covers it
	if strings.Contains(out, "claude: 50% used") {
		t.Errorf("ledger pace row for claude should be suppressed when live covers it, got: %s", out)
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
echo '{"pools":[{"key":"claude-5h","used":42,"stale":true,"reason":"ok"},{"key":"agy","used":10,"stale":false,"reason":"exhausted"}]}'`), 0755)

	orig := quotaBinPath
	defer func() { quotaBinPath = orig }()
	quotaBinPath = func() string { return script }

	l, _ := OpenLedger(filepath.Join(t.TempDir(), "ledger.json"))
	l.data["claude"] = Record{UsedPct: 20, WindowDays: intPtr(7), DaysLeft: intPtr(5), Updated: NowEpoch()}
	l.data["agy"] = Record{UsedPct: 10, WindowDays: intPtr(7), DaysLeft: intPtr(5), Updated: NowEpoch()}
	lc := NewLedgerCommands(l)
	out := lc.Advise()
	// stale pool should be skipped
	if strings.Contains(out, "claude-5h:") {
		t.Errorf("stale pool should not appear in live rows, got: %s", out)
	}
	// agy is valid (reason=exhausted, stale=false) but should not suppress ledger pace
	// because provider normalization maps agy->antigravity, and "agy" surface maps to "antigravity"
	if !strings.Contains(out, "agy:") {
		t.Errorf("agy ledger pace should still appear, got: %s", out)
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
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	jsonStr := `{"blocks": [{"isActive": true, "remainingMinutes": 240}]}`
	got := ParseRemainingMinutes(jsonStr)
	if got != 240 {
		t.Errorf("expected 240, got %d", got)
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
	os.Unsetenv("HERD_CREDITS_NO_REFRESH")

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
	}
}
