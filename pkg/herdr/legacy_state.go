package herdr

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// LegacyTabState is the durable migration record for a tab created before
// Herdr exposed immutable tab generations. A tombstone is deliberately not a
// close authorization: it only prevents the same unverifiable tab from
// producing identical BLOCKED evidence on every reconciliation cycle.
type LegacyTabState struct {
	Workspace string     `json:"workspace"`
	TabID     string     `json:"tab_id"`
	PaneID    string     `json:"pane_id,omitempty"`
	Action    string     `json:"action"` // backfill or tombstone
	Binding   TabBinding `json:"binding,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

const (
	legacyActionBackfill  = "backfill"
	legacyActionTombstone = "tombstone"
)

// LegacyTabStateStore is intentionally small so reconciliation tests can use
// a restartable file-backed store or an in-memory implementation.
type LegacyTabStateStore interface {
	Lookup(context.Context, string, string) (LegacyTabState, bool, error)
	Record(context.Context, LegacyTabState) error
}

// JSONLLegacyTabStateStore is append-only. The last record for an exact
// workspace/tab pair wins, making restart recovery deterministic and keeping
// interrupted writes from rewriting prior migration evidence.
type JSONLLegacyTabStateStore struct {
	Path string
	mu   sync.Mutex
}

func (s *JSONLLegacyTabStateStore) Lookup(_ context.Context, workspace, tabID string) (LegacyTabState, bool, error) {
	if s == nil || s.Path == "" {
		return LegacyTabState{}, false, fmt.Errorf("legacy tab state path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.Path)
	if os.IsNotExist(err) {
		return LegacyTabState{}, false, nil
	}
	if err != nil {
		return LegacyTabState{}, false, err
	}
	defer f.Close()
	var latest LegacyTabState
	found := false
	dec := json.NewDecoder(f)
	for {
		var state LegacyTabState
		if err := dec.Decode(&state); err != nil {
			if err == io.EOF {
				break
			}
			return LegacyTabState{}, false, fmt.Errorf("read legacy tab state: %w", err)
		}
		if state.Workspace == workspace && state.TabID == tabID {
			latest, found = state, true
		}
	}
	return latest, found, nil
}

func (s *JSONLLegacyTabStateStore) Record(_ context.Context, state LegacyTabState) error {
	if s == nil || s.Path == "" {
		return fmt.Errorf("legacy tab state path is required")
	}
	if state.Workspace == "" || state.TabID == "" {
		return fmt.Errorf("legacy tab state requires workspace and tab id")
	}
	if state.Action != legacyActionBackfill && state.Action != legacyActionTombstone {
		return fmt.Errorf("legacy tab state action %q is invalid", state.Action)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0755); err != nil {
		return fmt.Errorf("create legacy tab state directory: %w", err)
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("open legacy tab state: %w", err)
	}
	defer f.Close()
	b, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("marshal legacy tab state: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write legacy tab state: %w", err)
	}
	return f.Sync()
}
