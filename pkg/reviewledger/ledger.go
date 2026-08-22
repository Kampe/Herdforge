package reviewledger

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// quarantineSink is the package-level target for malformed JSONL lines.
// Set via SetQuarantinePath; if empty, malformed lines are silently dropped.
var (
	quarantineMu     sync.Mutex
	quarantinePath   string
	quarantineOpened bool
)

// NewReviewLedger creates a Ledger, deriving QueuePath from ledgerPath's directory.
func NewReviewLedger(repoRoot, ledgerPath string) (*Ledger, error) {
	dir := filepath.Dir(ledgerPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("create ledger dir: %w", err)
	}
	if _, err := os.Stat(ledgerPath); os.IsNotExist(err) {
		f, err := os.Create(ledgerPath)
		if err != nil {
			return nil, fmt.Errorf("create ledger: %w", err)
		}
		f.Close()
	}
	queuePath := QueuePathFor(ledgerPath)
	if _, err := os.Stat(queuePath); os.IsNotExist(err) {
		f, err := os.Create(queuePath)
		if err != nil {
			return nil, fmt.Errorf("create queue: %w", err)
		}
		f.Close()
	}
	return &Ledger{
		RepoRoot:     repoRoot,
		Path:         ledgerPath,
		QueuePath:    queuePath,
		Now:          time.Now,
		Coordinators: copyCoordSet(DefaultCoordinators),
	}, nil
}

// NewReadOnlyReviewLedger opens a ledger for queries without creating its
// parent directory, ledger file, or harvest queue. It is used by dry-run
// commands whose read-only contract must hold even on a fresh repository.
func NewReadOnlyReviewLedger(repoRoot, ledgerPath string) (*Ledger, error) {
	if info, err := os.Stat(ledgerPath); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("stat ledger: %w", err)
	} else if err == nil && info.IsDir() {
		return nil, fmt.Errorf("ledger path is a directory: %s", ledgerPath)
	}
	return &Ledger{
		RepoRoot:     repoRoot,
		Path:         ledgerPath,
		QueuePath:    QueuePathFor(ledgerPath),
		Now:          time.Now,
		Coordinators: copyCoordSet(DefaultCoordinators),
	}, nil
}

func copyCoordSet(src map[string]struct{}) map[string]struct{} {
	dst := make(map[string]struct{}, len(src))
	for k := range src {
		dst[k] = struct{}{}
	}
	return dst
}

func (l *Ledger) nowISO() string {
	return l.Now().UTC().Format(time.RFC3339Nano)
}

// appendRow writes a JSON row to the given file (append-only).
func (l *Ledger) appendRow(path string, row *LedgerRow) error {
	row.Timestamp = l.nowISO()
	data, err := json.Marshal(row)
	if err != nil {
		return fmt.Errorf("marshal row: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if _, err := f.WriteString("\n"); err != nil {
		return fmt.Errorf("write newline: %w", err)
	}
	return nil
}

// SetQuarantinePath enables quarantining of malformed JSONL lines.
func SetQuarantinePath(path string) {
	quarantineMu.Lock()
	quarantinePath = path
	quarantineOpened = false
	quarantineMu.Unlock()
}

// quarantineLine writes a malformed line to the quarantine file.
func quarantineLine(line string, reason error) {
	quarantineMu.Lock()
	defer quarantineMu.Unlock()
	if quarantinePath == "" {
		return
	}
	flag := os.O_APPEND | os.O_WRONLY
	if !quarantineOpened {
		flag |= os.O_CREATE
		quarantineOpened = true
	}
	f, err := os.OpenFile(quarantinePath, flag, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "# quarantine: %v\n%s\n", reason, line)
}

// readRows reads all rows from a JSONL file. Missing returns empty.
func readRows(path string) ([]LedgerRow, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read rows: %w", err)
	}
	defer f.Close()
	var rows []LedgerRow
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var row LedgerRow
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			quarantineLine(line, err)
			continue
		}
		rows = append(rows, row)
	}
	return rows, sc.Err()
}

// newestBy groups rows by key and keeps the newest per key.
func newestBy(rows []LedgerRow, key func(*LedgerRow) string) map[string]LedgerRow {
	out := make(map[string]LedgerRow)
	for i := range rows {
		k := key(&rows[i])
		out[k] = rows[i]
	}
	return out
}

// NormalizeSHA returns the full object ID from git rev-parse, or the input unchanged.
func (l *Ledger) NormalizeSHA(sha string) string {
	cmd := exec.Command("git", "-C", l.RepoRoot, "rev-parse", "--verify", "-q", sha+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		return sha
	}
	s := strings.TrimSpace(string(out))
	if s == "" {
		return sha
	}
	return s
}

// RejectCoordinatorSelfVerdict builds the error message when a coordinator self-verifies.
func RejectCoordinatorSelfVerdict(sha, reviewer string) error {
	return fmt.Errorf(
		"A coordinator self-verification never qualifies.\n"+
			"herd-review-ledger: refuse sha=%s reviewer=%s verdict=PASS",
		sha, reviewer,
	)
}

// ValidVerdict returns nil if v is one of the known verdict values.
func ValidVerdict(v Verdict) error {
	switch v {
	case VerdictPASS, VerdictFAIL, VerdictBLOCKED:
		return nil
	default:
		return fmt.Errorf("invalid verdict %q (must be PASS, FAIL, or BLOCKED)", string(v))
	}
}
