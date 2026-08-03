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
echo '{"claude":{"used":42,"pools":{"claude-5h":{"used":42,"remaining":58000,"resetsIn":"3h 0m","class":"onpace","stale":false,"reason":"ok"},"all":{"used":42,"stale":false,"reason":"ok"}}}}'`), 0755)

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
	if !strings.Contains(out, "  claude/claude-5h: 42% used, 58% left, onpace, reset 3h 0m") {
		t.Errorf("expected binding-format live row for claude/claude-5h, got: %s", out)
	}
	if !strings.Contains(out, "ledger-only fallback snapshots") {
		t.Errorf("expected ledger-only fallback header on live success, got: %s", out)
	}
	// claude should NOT get a ledger pace row because live covers it (by provider)
	if strings.Contains(out, "claude: 50% used") {
		t.Errorf("ledger pace row for claude should be suppressed when live covers provider, got: %s", out)
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
echo '{"claude":{"pools":{"claude-5h":{"used":42,"stale":true,"reason":"ok"}}},"antigravity":{"pools":{"agy":{"used":10,"resetsIn":"5d","class":"onpace","stale":false,"reason":"exhausted"}}}}'`), 0755)

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
	if !strings.Contains(out, "  antigravity/agy: 10% used, 90% left, onpace, reset 5d") {
		t.Errorf("expected binding-format live row for antigravity/agy, got: %s", out)
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

func TestRefresh_MalformedJSONLeavesManualSnapshot(t *testing.T) {
	dir := t.TempDir()
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
