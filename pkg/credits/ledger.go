package credits

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type AccountRow struct {
	Email          string `json:"email"`
	UsedPct        int    `json:"used_pct"`
	BurnOrder      int    `json:"burn_order"`
	Note           string `json:"note"`
	Updated        int64  `json:"updated"`
	ExhaustedUntil int64  `json:"exhausted_until,omitempty"`
	Source         string `json:"source,omitempty"`
}

type Record struct {
	UsedPct   int          `json:"used_pct"`
	WindowDays *int        `json:"window_days"`
	DaysLeft  *int         `json:"days_left"`
	Hourly    bool         `json:"hourly"`
	Note      string       `json:"note"`
	Updated   int64        `json:"updated"`
	Source    string       `json:"source,omitempty"`
	Accounts  []AccountRow `json:"accounts,omitempty"`
}

type Ledger struct {
	path  string
	data  map[string]Record
}

func OpenLedger(path string) (*Ledger, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("credits: mkdir ledger dir: %w", err)
	}
	data := make(map[string]Record)
	if _, err := os.Stat(path); err == nil {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("credits: read ledger: %w", err)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("credits: ledger file is empty (zero bytes)")
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "null" {
			return nil, fmt.Errorf("credits: ledger file contains JSON null")
		}
		if err := json.Unmarshal(raw, &data); err != nil {
			return nil, fmt.Errorf("credits: parse ledger: %w", err)
		}
		if data == nil {
			return nil, fmt.Errorf("credits: ledger file did not contain a JSON object")
		}
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("credits: stat ledger: %w", err)
	} else {
		raw := []byte("{}\n")
		if err := os.WriteFile(path, raw, 0644); err != nil {
			return nil, fmt.Errorf("credits: write empty ledger: %w", err)
		}
	}
	return &Ledger{path: path, data: data}, nil
}

func (l *Ledger) Surface(name string) (Record, error) {
	r, ok := l.data[name]
	if !ok {
		return Record{}, fmt.Errorf("credits: surface %q not found", name)
	}
	return r, nil
}

func (l *Ledger) WriteMutation(fn func(*map[string]Record)) error {
	// Deep-copy committed state so fn cannot mutate it before persistence;
	// the copy is swapped in only after the rename succeeds.
	working := make(map[string]Record, len(l.data))
	for k, v := range l.data {
		if v.WindowDays != nil {
			wd := *v.WindowDays
			v.WindowDays = &wd
		}
		if v.DaysLeft != nil {
			dl := *v.DaysLeft
			v.DaysLeft = &dl
		}
		if v.Accounts != nil {
			accts := make([]AccountRow, len(v.Accounts))
			copy(accts, v.Accounts)
			v.Accounts = accts
		}
		working[k] = v
	}

	fn(&working)

	raw, err := json.MarshalIndent(working, "", "  ")
	if err != nil {
		return fmt.Errorf("credits: marshal ledger: %w", err)
	}
	tmp := l.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0644); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("credits: write tmp ledger: %w", err)
	}
	if err := os.Rename(tmp, l.path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("credits: rename ledger: %w", err)
	}
	l.data = working
	return nil
}

func (l *Ledger) All() map[string]Record {
	return l.data
}

func (l *Ledger) Path() string {
	return l.path
}

func EffectiveUsed(rec Record) float64 {
	if len(rec.Accounts) > 0 {
		sum := 0.0
		for _, a := range rec.Accounts {
			sum += float64(a.UsedPct)
		}
		return sum / float64(len(rec.Accounts))
	}
	return float64(rec.UsedPct)
}

func PoolNote(rec Record) string {
	if len(rec.Accounts) == 0 {
		return rec.Note
	}
	sum := 0.0
	for _, a := range rec.Accounts {
		sum += float64(a.UsedPct)
	}
	eff := sum / float64(len(rec.Accounts))
	effTrunc := math.Floor(eff*10) / 10
	note := fmt.Sprintf("multi-account pool N=%d effective=%.1f%%", len(rec.Accounts), effTrunc)
	if rec.Note != "" {
		note += "; " + rec.Note
	}
	return note
}

func BuildPoolNote(accounts []AccountRow, callerNote string) string {
	if len(accounts) == 0 {
		return callerNote
	}
	sum := 0.0
	for _, a := range accounts {
		sum += float64(a.UsedPct)
	}
	eff := sum / float64(len(accounts))
	effTrunc := math.Floor(eff*10) / 10
	note := fmt.Sprintf("multi-account pool N=%d effective=%.1f%%", len(accounts), effTrunc)
	if callerNote != "" {
		note += "; " + callerNote
	}
	return note
}

func SortAccountsByBurnOrder(accounts []AccountRow) {
	sort.Slice(accounts, func(i, j int) bool {
		bi := accounts[i].BurnOrder
		if bi == 0 {
			bi = 99
		}
		bj := accounts[j].BurnOrder
		if bj == 0 {
			bj = 99
		}
		return bi < bj
	})
}
