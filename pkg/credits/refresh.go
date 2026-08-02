package credits

import (
	"fmt"
	"math"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

const (
	DefaultMax5hTokenBudget     = 20000000
	DefaultMaxWeeklyTokenBudget = 100000000
	DefaultRefreshTTL           = 60
	DefaultCcUsageTimeout       = 15
	DefaultPaceThrottleFloor    = 60
)

type RefreshResult struct {
	Output string
	Err    error
}

func NowEpoch() int64 {
	return time.Now().Unix()
}

var CcUsageBase = func() (string, error) {
	return "ccusage", nil
}

func CcUsageBlocks(surface string, timeoutSec int) string {
	if surface != "claude" {
		return ""
	}
	return execCcUsage([]string{"claude", "blocks", "--active", "--json"}, timeoutSec)
}

func CcUsageDaily(surface string, since string, timeoutSec int) string {
	if surface != "claude" {
		return ""
	}
	return execCcUsage([]string{"claude", "daily", "--since", since, "--json"}, timeoutSec)
}

func WeekAgoYYYMMDD() string {
	return time.Now().AddDate(0, 0, -7).Format("20060102")
}

func execCcUsage(args []string, timeoutSec int) string {
	base, err := CcUsageBase()
	if err != nil {
		return ""
	}
	cmdLine := strings.Join(append([]string{base}, args...), " ")
	cmd := exec.Command("sh", "-c", cmdLine)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type TokenParser func(jsonStr, jqExpr string) int

var ParseTokensFromJSON TokenParser = func(jsonStr, jqExpr string) int {
	if jsonStr == "" {
		return 0
	}
	return 0
}

var ParseRemainingMinutes func(jsonStr string) int = func(jsonStr string) int {
	return 0
}

func Refresh(ledger *Ledger, surface string, budget5h, budgetWeekly, ttl int, emails []string) *RefreshResult {
	if surface != "claude" {
		return &RefreshResult{
			Output: fmt.Sprintf("herd-credits: refresh only wired for claude (ccusage has no data for %s)", surface),
		}
	}

	noRefresh := os.Getenv("HERD_CREDITS_NO_REFRESH")
	if noRefresh == "1" {
		return &RefreshResult{Output: "herd-credits: refresh skipped (HERD_CREDITS_NO_REFRESH=1)"}
	}

	activeEmail := ClaudeActiveEmail("")
	if activeEmail == "" {
		activeEmail = ClaudeActiveExpanded()
	}
	if activeEmail == "" {
		return &RefreshResult{Output: "herd-credits: refresh skipped (no active claude account known)"}
	}

	blocksJSON := CcUsageBlocks(surface, DefaultCcUsageTimeout)
	dailyJSON := CcUsageDaily(surface, WeekAgoYYYMMDD(), DefaultCcUsageTimeout)

	if blocksJSON == "" && dailyJSON == "" {
		return &RefreshResult{Output: "herd-credits: ccusage unavailable, using manual snapshot"}
	}

	tok5h := ParseTokensFromJSON(blocksJSON, ".blocks[]? | select(.isActive == true) | (.totalTokens // 0)")
	if tok5h < 0 {
		tok5h = 0
	}
	tokWeek := ParseTokensFromJSON(dailyJSON, ".totals.totalTokens // 0")
	if tokWeek < 0 {
		tokWeek = 0
	}

	if budget5h <= 0 {
		budget5h = DefaultMax5hTokenBudget
	}
	if budgetWeekly <= 0 {
		budgetWeekly = DefaultMaxWeeklyTokenBudget
	}

	pct5h := tok5h * 100 / budget5h
	if pct5h > 100 {
		pct5h = 100
	}
	pctWeek := tokWeek * 100 / budgetWeekly
	if pctWeek > 100 {
		pctWeek = 100
	}

	hourlyDead := 0
	untilEpoch := int64(0)
	if pct5h >= 95 {
		remMin := ParseRemainingMinutes(blocksJSON)
		if remMin <= 0 {
			remMin = 60
		}
		holdHours := (remMin + 59) / 60
		if holdHours < 1 {
			holdHours = 1
		}
		hourlyDead = 1
		untilEpoch = NowEpoch() + int64(holdHours)*3600
	}

	mutationErr := ledger.WriteMutation(func(m *map[string]Record) {
		data := *m
		rec, exists := data[surface]
		if !exists {
			wd := 7
			dl := 7
			rec = Record{
				UsedPct:    0,
				WindowDays: &wd,
				DaysLeft:   &dl,
			}
		}

		if len(rec.Accounts) > 0 {
			found := false
			for i, a := range rec.Accounts {
				if strings.EqualFold(a.Email, activeEmail) {
					rec.Accounts[i].UsedPct = pctWeek
					rec.Accounts[i].Updated = NowEpoch()
					rec.Accounts[i].Note = "ccusage"
					if hourlyDead == 1 {
						rec.Accounts[i].ExhaustedUntil = untilEpoch
					} else {
						rec.Accounts[i].ExhaustedUntil = 0
					}
					found = true
					break
				}
			}
			if !found {
				bo := 99
				acct := AccountRow{
					Email:     activeEmail,
					UsedPct:   pctWeek,
					BurnOrder: bo,
					Updated:   NowEpoch(),
					Note:      "ccusage",
				}
				if hourlyDead == 1 {
					acct.ExhaustedUntil = untilEpoch
				}
				rec.Accounts = append(rec.Accounts, acct)
			}
			SortAccountsByBurnOrder(rec.Accounts)
			if len(rec.Accounts) > 0 {
				rec.UsedPct = rec.Accounts[0].UsedPct
			}
		} else {
			rec.UsedPct = pctWeek
			rec.Updated = NowEpoch()
			rec.Source = "ccusage"
		}
		rec.Note = BuildPoolNote(rec.Accounts, "ccusage")
		data[surface] = rec
	})

	if mutationErr != nil {
		return &RefreshResult{Err: mutationErr}
	}

	hourlyDeadStr := ""
	if hourlyDead == 1 {
		hourlyDeadStr = " (hourly-dead)"
	}
	return &RefreshResult{
		Output: fmt.Sprintf("herd-credits: %s live refresh: account %s weekly=%d%% 5h=%d%%%s",
			surface, activeEmail, pctWeek, pct5h, hourlyDeadStr),
	}
}

func MaybeRefresh(ledger *Ledger) *RefreshResult {
	autoRefresh := os.Getenv("HERD_CREDITS_AUTO_REFRESH")
	if autoRefresh != "1" {
		return nil
	}
	noRefresh := os.Getenv("HERD_CREDITS_NO_REFRESH")
	if noRefresh == "1" {
		return nil
	}

	rec, err := ledger.Surface("claude")
	if err != nil {
		return Refresh(ledger, "claude", DefaultMax5hTokenBudget, DefaultMaxWeeklyTokenBudget, DefaultRefreshTTL, nil)
	}
	age := NowEpoch() - rec.Updated
	if age >= int64(DefaultRefreshTTL) {
		return Refresh(ledger, "claude", DefaultMax5hTokenBudget, DefaultMaxWeeklyTokenBudget, DefaultRefreshTTL, nil)
	}
	return nil
}

type LedgerCommands struct {
	Ledger *Ledger
}

func NewLedgerCommands(ledger *Ledger) *LedgerCommands {
	return &LedgerCommands{Ledger: ledger}
}

func (lc *LedgerCommands) Set(surface string, used, windowDays, daysLeft int, hourly bool, note string) (string, error) {
	activeEmail := ClaudeActiveEmail("")

	mutationErr := lc.Ledger.WriteMutation(func(m *map[string]Record) {
		data := *m
		wd := windowDays
		dl := daysLeft
		rec := Record{
			UsedPct:    used,
			WindowDays: &wd,
			DaysLeft:   &dl,
			Hourly:     hourly,
			Note:       note,
			Updated:    NowEpoch(),
		}

		existing, exists := data[surface]
		if exists && len(existing.Accounts) > 0 {
			rec.Accounts = make([]AccountRow, len(existing.Accounts))
			copy(rec.Accounts, existing.Accounts)
			if activeEmail != "" {
				for i, a := range rec.Accounts {
					if strings.EqualFold(a.Email, activeEmail) {
						rec.Accounts[i].UsedPct = used
						rec.Accounts[i].Updated = NowEpoch()
						break
					}
				}
			}
		}
		data[surface] = rec
	})
	if mutationErr != nil {
		return "", mutationErr
	}

	out := lc.Pace(surface)
	if activeEmail != "" {
		out = fmt.Sprintf("herd-credits: %s active account %s used=%d%%\n%s", surface, activeEmail, used, out)
	}
	return out, nil
}

func (lc *LedgerCommands) AccountSet(surface, email string, used, burnOrder int, note string) (string, error) {
	if email == "" {
		return "", fmt.Errorf("credits: account-set needs --email")
	}
	if burnOrder <= 0 {
		burnOrder = 99
	}

	mutationErr := lc.Ledger.WriteMutation(func(m *map[string]Record) {
		data := *m

		existing, exists := data[surface]
		if !exists {
			wd := 7
			dl := 7
			existing = Record{
				UsedPct:    0,
				WindowDays: &wd,
				DaysLeft:   &dl,
			}
		}

		newAccounts := make([]AccountRow, 0)
		for _, a := range existing.Accounts {
			if !strings.EqualFold(a.Email, email) {
				newAccounts = append(newAccounts, a)
			}
		}

		newAccounts = append(newAccounts, AccountRow{
			Email:     email,
			UsedPct:   used,
			BurnOrder: burnOrder,
			Note:      note,
			Updated:   NowEpoch(),
		})

		SortAccountsByBurnOrder(newAccounts)
		existing.Accounts = newAccounts
		existing.Updated = NowEpoch()

		if len(existing.Accounts) > 0 {
			existing.UsedPct = existing.Accounts[0].UsedPct
		}

		existing.Note = BuildPoolNote(existing.Accounts, note)
		data[surface] = existing
	})

	if mutationErr != nil {
		return "", mutationErr
	}

	return fmt.Sprintf("herd-credits: %s account %s used=%d%% burn_order=%d", surface, email, used, burnOrder), nil
}

func (lc *LedgerCommands) AccountExhausted(surface, email string, hoursLeft int, clear bool) (string, error) {
	if email == "" {
		return "", fmt.Errorf("credits: account-exhausted needs --email")
	}

	mutationErr := lc.Ledger.WriteMutation(func(m *map[string]Record) {
		data := *m
		rec, exists := data[surface]
		if !exists {
			return
		}

		newAccounts := make([]AccountRow, 0)
		for _, a := range rec.Accounts {
			if strings.EqualFold(a.Email, email) {
				a.Updated = NowEpoch()
				if clear {
					a.ExhaustedUntil = 0
				} else {
					a.ExhaustedUntil = NowEpoch() + int64(hoursLeft)*3600
				}
			}
			newAccounts = append(newAccounts, a)
		}
		rec.Accounts = newAccounts
		rec.Updated = NowEpoch()
		data[surface] = rec
	})
	if mutationErr != nil {
		return "", mutationErr
	}

	if clear {
		return fmt.Sprintf("herd-credits: %s account %s hourly exhaustion cleared", surface, email), nil
	}
	return fmt.Sprintf("herd-credits: %s account %s HOURLY-DEAD for ~%dh (pick will skip it)", surface, email, hoursLeft), nil
}

func (lc *LedgerCommands) Exhausted(surface string, hoursLeft int) (string, error) {
	mutationErr := lc.Ledger.WriteMutation(func(m *map[string]Record) {
		data := *m
		rec, exists := data[surface]
		if !exists {
			rec = Record{}
		}
		rec.UsedPct = 100
		rec.WindowDays = nil
		rec.DaysLeft = nil
		rec.Note = fmt.Sprintf("exhausted, resets in ~%dh", hoursLeft)
		rec.Updated = NowEpoch()

		for i := range rec.Accounts {
			rec.Accounts[i].UsedPct = 100
			rec.Accounts[i].Updated = NowEpoch()
		}
		data[surface] = rec
	})
	if mutationErr != nil {
		return "", mutationErr
	}
	return fmt.Sprintf("herd-credits: %s marked exhausted (~%dh). Also set a herd-route cooldown for the hard gate.", surface, hoursLeft), nil
}

func (lc *LedgerCommands) Pace(surface string) string {
	rec, err := lc.Ledger.Surface(surface)
	if err != nil {
		return fmt.Sprintf("%s: untracked (treat as available; watch panes for quota errors)", surface)
	}

	nAcct := len(rec.Accounts)
	usedI := PoolEffectiveCapped(rec)

	if rec.WindowDays == nil {
		return fmt.Sprintf("%s: exhausted -> concurrency 0", surface)
	}

	window := *rec.WindowDays
	left := 0
	if rec.DaysLeft != nil {
		left = *rec.DaysLeft
	}
	elapsed := (window - left) * 100 / window

	floorPct := DefaultPaceThrottleFloor
	if f := os.Getenv("HERD_PACE_THROTTLE_FLOOR"); f != "" {
		fmt.Sscanf(f, "%d", &floorPct)
	}

	cls := PaceClassOf(usedI, elapsed, floorPct)

	if nAcct > 0 {
		out := fmt.Sprintf("%s: effective %d%% of %dx100%% pool, %d%% window elapsed -> %s -> concurrency %d",
			surface, usedI, nAcct, elapsed, cls, ClassConcurrency(cls))

		SortAccountsByBurnOrder(rec.Accounts)
		for _, a := range rec.Accounts {
			until := ""
			if a.ExhaustedUntil > NowEpoch() {
				until = " [HOURLY-DEAD]"
			}
			out += fmt.Sprintf("\n  burn#%d %s used=%d%%%s", a.BurnOrder, a.Email, a.UsedPct, until)
		}

		// primary = burn-order-1; check exhaustion and pause
		var primary, reserve AccountRow
		for _, a := range rec.Accounts {
			if a.BurnOrder == 1 || (a.BurnOrder == 0 && primary.Email == "") {
				primary = a
			}
		}
		for _, a := range rec.Accounts {
			if a.Email != primary.Email && reserve.Email == "" {
				reserve = a
			}
		}

		primaryExhausted := primary.UsedPct >= 95 || primary.ExhaustedUntil > NowEpoch()
		primaryPaused := strings.Contains(primary.Note, "PAUSED") || strings.Contains(primary.Note, "do-not-dispatch")

		if primaryPaused {
			out += fmt.Sprintf("\n  PAUSED: primary burn#%d %s is paused (note: %s)", primary.BurnOrder, primary.Email, primary.Note)
		}
		if primaryExhausted && reserve.Email != "" {
			want := ClaudeEmailToAccount(reserve.Email)
			out += fmt.Sprintf("\n  SWITCH_AUTH: primary %s exhausted; reserve %s has headroom", primary.Email, reserve.Email)
			out += fmt.Sprintf("\n  ACTION: claude-account use %s", want)
			out += "\n  (or: bin/herd-route --auto-swap-auth  when no claude session is mid-task)"
		}

		active := ClaudeActiveEmail("")
		if active == "" {
			active = ClaudeActiveExpanded()
		}
		if active != "" {
			out += fmt.Sprintf("\n  active CLI (sidecar/auth): %s  [account=%s]", active, ClaudeEmailToAccount(active))
		}

		return out
	}

	return fmt.Sprintf("%s: %d%% used, %d%% of window elapsed -> %s -> concurrency %d",
		surface, usedI, elapsed, cls, ClassConcurrency(cls))
}

func (lc *LedgerCommands) Show() string {
	MaybeRefresh(lc.Ledger)

	lines := make([]string, 0)
	surfaces := make([]string, 0)
	for k := range lc.Ledger.All() {
		surfaces = append(surfaces, k)
	}
	sort.Strings(surfaces)

	for _, s := range surfaces {
		rec := lc.Ledger.All()[s]
		if len(rec.Accounts) > 0 {
			eff := EffectiveUsed(rec)
			lines = append(lines, fmt.Sprintf("%s: multi N=%d effective=%.0f%%  [%s]", s, len(rec.Accounts), math.Floor(eff), rec.Note))
		} else {
			dl := "-"
			if rec.DaysLeft != nil {
				dl = fmt.Sprintf("%d", *rec.DaysLeft)
			}
			wd := "-"
			if rec.WindowDays != nil {
				wd = fmt.Sprintf("%d", *rec.WindowDays)
			}
			lines = append(lines, fmt.Sprintf("%s: %d%% used, %s/%sd left  [%s]", s, rec.UsedPct, dl, wd, rec.Note))
		}
	}
	return strings.Join(lines, "\n")
}

func (lc *LedgerCommands) Advise() string {
	MaybeRefresh(lc.Ledger)

	quotaBin := os.Getenv("HERD_QUOTA_BIN")
	if quotaBin == "" {
		quotaBin = "bin/herd-quota"
	}

	var out strings.Builder

	if _, err := os.Stat(quotaBin); err == nil {
		cmd := exec.Command("sh", "-c", quotaBin+" --json 2>/dev/null || true")
		if liveOut, err := cmd.Output(); err == nil {
			liveStr := strings.TrimSpace(string(liveOut))
			if liveStr != "" {
				out.WriteString("live OpenUsage quota (authoritative for routing):\n")
				out.WriteString(liveStr)
				out.WriteString("\n")
			}
		}
	}

	out.WriteString("ledger-only fallback snapshots (used only when live quota is unavailable):\n")
	surfaces := make([]string, 0)
	for k := range lc.Ledger.All() {
		surfaces = append(surfaces, k)
	}
	sort.Strings(surfaces)

	for _, s := range surfaces {
		out.WriteString(lc.Pace(s) + "\n")
	}

	out.WriteString("untracked surfaces: available; enter a snapshot when a quota UI or error is seen\n")
	out.WriteString("Claude auth-account selection metadata (ledger, not routing quota):\n")
	out.WriteString("  pool avg = sum(used)/N; pick by account headroom every launch (resets differ)\n")

	active := ClaudeActiveEmail("")
	if active == "" {
		active = ClaudeActiveExpanded()
	}
	if active != "" {
		out.WriteString(fmt.Sprintf("claude-account current sidecar: %s (%s)\n", active, ClaudeEmailToAccount(active)))
	}

	if rec, err := lc.Ledger.Surface("claude"); err == nil && len(rec.Accounts) > 0 {
		SortAccountsByBurnOrder(rec.Accounts)
		var primary, reserve string
		for _, a := range rec.Accounts {
			if a.UsedPct >= 95 && primary == "" {
				primary = a.Email
			}
		}
		for _, a := range rec.Accounts {
			if a.UsedPct < 95 && reserve == "" {
				reserve = a.Email
			}
		}
		if primary != "" && reserve != "" {
			want := ClaudeEmailToAccount(reserve)
			out.WriteString(fmt.Sprintf("READY SWAP (primary exhausted): claude-account use %s\n", want))
		}
	}

	return out.String()
}

func (lc *LedgerCommands) AccountList(surface string) string {
	rec, err := lc.Ledger.Surface(surface)
	if err != nil {
		return fmt.Sprintf("no surface %s", surface)
	}
	if len(rec.Accounts) == 0 {
		return fmt.Sprintf("%s: single-account used=%d%%", surface, rec.UsedPct)
	}

	SortAccountsByBurnOrder(rec.Accounts)
	var out strings.Builder
	eff := EffectiveUsed(rec)
	out.WriteString(fmt.Sprintf("%s: multi-account N=%d effective=%.0f%%\n", surface, len(rec.Accounts), eff))

	for _, a := range rec.Accounts {
		untilStr := ""
		if a.ExhaustedUntil > 0 {
			remMin := (a.ExhaustedUntil - NowEpoch()) / 60
			if remMin > 0 {
				untilStr = fmt.Sprintf("  HOURLY-DEAD ~%dm", remMin)
			}
		}
		out.WriteString(fmt.Sprintf("  burn#%d  %s  used=%d%%%s  %s\n", a.BurnOrder, a.Email, a.UsedPct, untilStr, a.Note))
	}

	active := ClaudeActiveEmail("")
	if active == "" {
		active = ClaudeActiveExpanded()
	}
	if active != "" {
		out.WriteString(fmt.Sprintf("active claude CLI auth: %s\n", active))
	}

	return strings.TrimRight(out.String(), "\n")
}

func (lc *LedgerCommands) Selftest() string {
	const throttleFloor = 60

	tt := []struct {
		used, elapsed, floor int
		want                PaceClass
	}{
		{32, 14, throttleFloor, ClassOnpace},
		{70, 14, throttleFloor, ClassOverpace},
		{32, 14, 20, ClassOverpace},
		{2, 0, throttleFloor, ClassUnderspent},
		{50, 50, throttleFloor, ClassOnpace},
		{96, 50, throttleFloor, ClassExhausted},
	}

	for _, tc := range tt {
		got := PaceClassOf(tc.used, tc.elapsed, tc.floor)
		if got != tc.want {
			return fmt.Sprintf("FAIL pace_class(%d,%d,floor=%d): got %s, want %s", tc.used, tc.elapsed, tc.floor, got, tc.want)
		}
	}

	cc := map[PaceClass]int{
		ClassExhausted:  0,
		ClassOverpace:   1,
		ClassOnpace:     2,
		ClassUnderspent: 3,
	}
	for cls, want := range cc {
		got := ClassConcurrency(cls)
		if got != want {
			return fmt.Sprintf("FAIL concurrency(%s): got %d, want %d", cls, got, want)
		}
	}

	multiLedger := &Ledger{data: map[string]Record{
		"claude": {
			Accounts: []AccountRow{
				{Email: "a@x", UsedPct: 33, BurnOrder: 1},
				{Email: "b@y", UsedPct: 0, BurnOrder: 2},
			},
			WindowDays: intPtr(7),
			DaysLeft:   intPtr(6),
		},
	}}

	rec, _ := multiLedger.Surface("claude")
	eff := EffectiveUsed(rec)
	if eff <= 16 || eff >= 17 {
		return fmt.Sprintf("FAIL multi-account average: got %.2f, expected ~16.5", eff)
	}

	return "herd-credits selftest: PASS"
}

func intPtr(i int) *int { return &i }

func ClaudeActiveExpanded() string {
	return ""
}
