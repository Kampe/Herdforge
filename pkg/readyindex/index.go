// Package readyindex is the FAC-603 exact-ready set for merge discovery.
//
// Determining what is ready to merge must be an indexed lookup, not a full
// scan of every tip in every worktree. Cost of a full drain grows with fleet
// history; this index is updated on verdict ingest and candidate state change,
// so the default drain path stays bounded by the ready set size.
//
// The harvest queue remains append-only audit truth. This file is a compacted
// projection: List falls back to rebuilding from the queue when the projection
// is missing or corrupt. Index presence is never harvest authority — Admit,
// review cap, and provenance gates are unchanged.
package readyindex

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// Leaf is the compacted exact-ready projection beside the review ledger.
	Leaf   = "ready-index.json"
	Schema = 1
)

// Entry is one exact-ready candidate. Ready means "PASS admitted and queued";
// freshness/conflict classification still happens at drain time over this set.
type Entry struct {
	SHA      string `json:"sha"`
	Branch   string `json:"branch,omitempty"`
	Lane     string `json:"lane,omitempty"`
	Reviewer string `json:"reviewer,omitempty"`
	Updated  string `json:"updated_at"`
}

// Index is the durable compacted exact-ready projection.
type Index struct {
	SchemaVersion int     `json:"schema_version"`
	UpdatedAt     string  `json:"updated_at"`
	Source        string  `json:"source"` // ingest | repair | rebuild
	Entries       []Entry `json:"entries"`
}

// PathFor derives the ready-index path from a review-ledger path.
func PathFor(ledgerPath string) string {
	return filepath.Join(filepath.Dir(strings.TrimSpace(ledgerPath)), Leaf)
}

// Path joins repoRoot with .herd/ready-index.json (cwd-relative when root empty).
func Path(repoRoot string) string {
	root := strings.TrimSpace(repoRoot)
	if root == "" {
		return filepath.Join(".herd", Leaf)
	}
	return filepath.Join(root, ".herd", Leaf)
}

// Upsert records or refreshes one exact-ready entry after a PASS enqueue.
func Upsert(indexPath string, e Entry) error {
	e.SHA = strings.TrimSpace(e.SHA)
	if e.SHA == "" {
		return fmt.Errorf("readyindex: upsert requires sha")
	}
	if strings.TrimSpace(e.Updated) == "" {
		e.Updated = time.Now().UTC().Format(time.RFC3339Nano)
	}
	idx, err := loadOrEmpty(indexPath)
	if err != nil {
		return err
	}
	found := false
	for i := range idx.Entries {
		if idx.Entries[i].SHA == e.SHA {
			idx.Entries[i] = e
			found = true
			break
		}
	}
	if !found {
		idx.Entries = append(idx.Entries, e)
	}
	sort.Slice(idx.Entries, func(i, j int) bool { return idx.Entries[i].SHA < idx.Entries[j].SHA })
	idx.Source = "ingest"
	return write(indexPath, idx)
}

// Remove drops a SHA after revoke or consume.
func Remove(indexPath, sha string) error {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return fmt.Errorf("readyindex: remove requires sha")
	}
	idx, err := loadOrEmpty(indexPath)
	if err != nil {
		return err
	}
	out := idx.Entries[:0]
	for _, e := range idx.Entries {
		if e.SHA != sha {
			out = append(out, e)
		}
	}
	idx.Entries = out
	idx.Source = "ingest"
	return write(indexPath, idx)
}

// List returns the compacted entries. os.ErrNotExist means no projection yet.
func List(indexPath string) ([]Entry, error) {
	idx, err := Load(indexPath)
	if err != nil {
		return nil, err
	}
	return append([]Entry(nil), idx.Entries...), nil
}

// Load reads the projection file.
func Load(indexPath string) (*Index, error) {
	body, err := os.ReadFile(indexPath)
	if err != nil {
		return nil, err
	}
	var idx Index
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, fmt.Errorf("readyindex: corrupt index: %w", err)
	}
	return &idx, nil
}

// Rebuild replaces the projection from an authoritative entry list (repair /
// reconcile from Queued()). Bounded by entry count, never by worktree count.
func Rebuild(indexPath string, entries []Entry, source string) error {
	if strings.TrimSpace(source) == "" {
		source = "rebuild"
	}
	cp := make([]Entry, 0, len(entries))
	seen := map[string]bool{}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, e := range entries {
		e.SHA = strings.TrimSpace(e.SHA)
		if e.SHA == "" || seen[e.SHA] {
			continue
		}
		seen[e.SHA] = true
		if strings.TrimSpace(e.Updated) == "" {
			e.Updated = now
		}
		cp = append(cp, e)
	}
	sort.Slice(cp, func(i, j int) bool { return cp[i].SHA < cp[j].SHA })
	return write(indexPath, &Index{
		SchemaVersion: Schema,
		UpdatedAt:     now,
		Source:        source,
		Entries:       cp,
	})
}

func loadOrEmpty(indexPath string) (*Index, error) {
	idx, err := Load(indexPath)
	if err == nil {
		return idx, nil
	}
	if !os.IsNotExist(err) {
		// Corrupt projection: start empty rather than refuse ingest.
		return &Index{SchemaVersion: Schema}, nil
	}
	return &Index{SchemaVersion: Schema}, nil
}

func write(indexPath string, idx *Index) error {
	if idx.SchemaVersion == 0 {
		idx.SchemaVersion = Schema
	}
	idx.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.MkdirAll(filepath.Dir(indexPath), 0o700); err != nil {
		return fmt.Errorf("readyindex: mkdir: %w", err)
	}
	body, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	tmp := indexPath + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("readyindex: write: %w", err)
	}
	if err := os.Rename(tmp, indexPath); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("readyindex: rename: %w", err)
	}
	return nil
}
