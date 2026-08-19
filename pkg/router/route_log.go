package router

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var routeLogMu sync.Mutex

// AppendRouteDecision records one routing decision as a queryable JSONL
// event. The timestamp is part of the event rather than inferred from file
// metadata so callers can filter and correlate decisions after the fact.
//
// The append is serialized within a process and synced before returning. A
// caller that requires a durable route must treat any returned error as a
// hard failure and avoid presenting the decision as successful.
func AppendRouteDecision(path string, decision *Route, now func() time.Time) error {
	if decision == nil {
		return fmt.Errorf("route decision log: nil decision")
	}
	if path == "" {
		return fmt.Errorf("route decision log: empty path")
	}
	if now == nil {
		now = time.Now
	}

	entry := struct {
		Timestamp time.Time `json:"timestamp"`
		Route
	}{Timestamp: now().UTC(), Route: *decision}
	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("route decision log: marshal: %w", err)
	}
	data = append(data, '\n')

	routeLogMu.Lock()
	defer routeLogMu.Unlock()

	if parent := filepath.Dir(path); parent != "." {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return fmt.Errorf("route decision log: create parent: %w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("route decision log: open %q: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return fmt.Errorf("route decision log: append %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("route decision log: sync %q: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("route decision log: close %q: %w", path, err)
	}
	return nil
}
