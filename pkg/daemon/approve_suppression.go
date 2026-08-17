package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provider"
)

const (
	approveSuppressionVersion = 1
	approveSuppressionLimit   = 128
)

// approveSuppression is a bounded, durable tombstone for one exact legacy
// refusal. State is part of the identity: a changed board revision or receipt
// must never inherit a refusal for an older candidate.
type approveSuppression struct {
	Key            string `json:"key"`
	Ref            string `json:"ref"`
	Reason         string `json:"reason"`
	CandidateState string `json:"candidate_state"`
	ReceiptState   string `json:"receipt_state"`
	BlockedAt      string `json:"blocked_at"`
}

type approveSuppressionFile struct {
	Version int                  `json:"version"`
	Entries []approveSuppression `json:"entries"`
}

type approveSuppressionLedger struct {
	path    string
	entries map[string]approveSuppression
}

func loadApproveSuppressionLedger(path string) (*approveSuppressionLedger, error) {
	l := &approveSuppressionLedger{path: path, entries: make(map[string]approveSuppression)}
	if strings.TrimSpace(path) == "" {
		return l, nil
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return l, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read approve suppression ledger: %w", err)
	}
	var f approveSuppressionFile
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("decode approve suppression ledger: %w", err)
	}
	if f.Version != approveSuppressionVersion {
		return nil, fmt.Errorf("unsupported approve suppression ledger version %d", f.Version)
	}
	for _, entry := range f.Entries {
		if entry.Key != "" && entry.Ref != "" {
			l.entries[entry.Key] = entry
		}
	}
	return l, nil
}

func (l *approveSuppressionLedger) has(key string) bool {
	_, ok := l.entries[key]
	return ok
}

func (l *approveSuppressionLedger) record(entry approveSuppression) error {
	if l.path == "" {
		l.entries[entry.Key] = entry
		return nil
	}
	l.entries[entry.Key] = entry
	return l.persist()
}

func (l *approveSuppressionLedger) removeRef(ref string) error {
	changed := false
	for key, entry := range l.entries {
		if entry.Ref == ref {
			delete(l.entries, key)
			changed = true
		}
	}
	if changed && l.path != "" {
		return l.persist()
	}
	return nil
}

func (l *approveSuppressionLedger) persist() error {
	entries := make([]approveSuppression, 0, len(l.entries))
	for _, entry := range l.entries {
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].BlockedAt != entries[j].BlockedAt {
			return entries[i].BlockedAt > entries[j].BlockedAt
		}
		return entries[i].Key < entries[j].Key
	})
	if len(entries) > approveSuppressionLimit {
		entries = entries[:approveSuppressionLimit]
		l.entries = make(map[string]approveSuppression, len(entries))
		for _, entry := range entries {
			l.entries[entry.Key] = entry
		}
	}
	payload, err := json.MarshalIndent(approveSuppressionFile{Version: approveSuppressionVersion, Entries: entries}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode approve suppression ledger: %w", err)
	}
	dir := filepath.Dir(l.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create approve suppression directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".approve-suppressions-*.tmp")
	if err != nil {
		return fmt.Errorf("create approve suppression temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod approve suppression temporary file: %w", err)
	}
	if _, err := tmp.Write(append(payload, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write approve suppression ledger: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close approve suppression ledger: %w", err)
	}
	if err := os.Rename(tmpName, l.path); err != nil {
		return fmt.Errorf("replace approve suppression ledger: %w", err)
	}
	return nil
}

func approveSuppressionState(t *provider.Task) (candidateState, receiptState string) {
	if t == nil {
		return "nil-task", "nil-receipt"
	}
	candidate := strings.Join([]string{t.ID, t.Status, t.UpdatedAt.UTC().Format(time.RFC3339Nano), t.Description}, "\x1f")
	receipt := strings.Join([]string{t.StatusReceipt, t.UpdatedAt.UTC().Format(time.RFC3339Nano)}, "\x1f")
	candidateSum := sha256.Sum256([]byte(candidate))
	receiptSum := sha256.Sum256([]byte(receipt))
	return hex.EncodeToString(candidateSum[:]), hex.EncodeToString(receiptSum[:])
}

func approveSuppressionKey(ref, reason, candidateState, receiptState string) string {
	return strings.Join([]string{ref, reason, candidateState, receiptState}, "\x1f")
}
