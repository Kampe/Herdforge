package credits

import (
	"fmt"
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
echo '{"claude":{"claude-5h":{"used":42,"remaining":58000,"resetsIn":180,"class":"onpace","stale":false,"reason":"ok"},"all":{"used":42,"stale":false,"reason":"ok"}}}'`), 0755)

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
	if !strings.Contains(out, "claude/claude-5h:") {
		t.Errorf("expected live pool row for claude/claude-5h, got: %s", out)
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
	if !strings.Contains(out, "resets in 180") {
		t.Errorf("expected resetsIn=180, got: %s", out)
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
echo '{"claude":{"claude-5h":{"used":42,"stale":true,"reason":"ok"}},"antigravity":{"agy":{"used":10,"stale":false,"reason":"exhausted"}}}'`), 0755)

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
	// agy is valid (reason=exhausted, stale=false) so it should appear as live row
	// but agy maps to antigravity, so the "agy" ledger surface should NOT be suppressed
	// because "agy" normalizes to "antigravity" which IS covered
	if !strings.Contains(out, "antigravity/agy:") {
		t.Errorf("expected live row for antigravity/agy, got: %s", out)
	}
	if strings.Contains(out, "agy: 10% used, 28% of window") {
		t.Errorf("agy ledger pace should be suppressed when live covers antigravity, got: %s", out)
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
	os.Unsetenv("HERD_CREDITS_NO_REFRESH")

	l, _ := OpenLedger(filepath.Join(dir, "ledger.json"))
	r := Refresh(l, "claude", 0, 0, 0, nil)
	if r.Err != nil {
		t.Fatalf("unexpected err: %v", r.Err)
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
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}

	dir := t.TempDir()
	os.Setenv("HERD_CREDITS_NO_REFRESH", "")
	defer os.Unsetenv("HERD_CREDITS_NO_REFRESH")

	// mock sidecar to return an active email
	origAccountsDir := os.Getenv("HERD_CLAUDE_ACCOUNTS_DIR")
	os.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	defer func() { os.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", origAccountsDir) }()
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

func TestRefresh_MalformedJSONLeavesManualSnapshot(t *testing.T) {
	dir := t.TempDir()
	os.Setenv("HERD_CREDITS_NO_REFRESH", "")
	defer os.Unsetenv("HERD_CREDITS_NO_REFRESH")

	origAccountsDir := os.Getenv("HERD_CLAUDE_ACCOUNTS_DIR")
	os.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", dir)
	defer func() { os.Setenv("HERD_CLAUDE_ACCOUNTS_DIR", origAccountsDir) }()
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

func TestCcUsageBase_PATHFallback(t *testing.T) {
	// This test verifies CcUsageBase returns "ccusage" if it's on PATH,
	// or "npx" if only npx is available, or error if neither.
	// We can't easily manipulate PATH in-process, so we test the contract
	// by calling it and checking it returns one of the expected values.
	base, err := CcUsageBase()
	if err != nil {
		// neither ccusage nor npx on PATH — that's a valid result
		return
	}
	if base != "ccusage" && base != "npx" {
		t.Errorf("CcUsageBase returned unexpected value: %q", base)
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
