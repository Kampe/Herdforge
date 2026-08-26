package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var routeLogMu sync.Mutex

type routeLogEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Route
}

func validateRouteDecision(decision *Route) error {
	if decision.Provider == "" {
		return fmt.Errorf("route decision log: empty provider")
	}
	if _, ok := SurfaceFor(decision.Provider); !ok {
		return fmt.Errorf("route decision log: unknown provider %q", decision.Provider)
	}
	if !validShapes[decision.Task] {
		return fmt.Errorf("route decision log: unknown task shape %q", decision.Task)
	}
	return nil
}

func validateRouteLog(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		return fmt.Errorf("route decision log: incomplete trailing record")
	}
	for _, line := range bytes.Split(data[:len(data)-1], []byte{'\n'}) {
		if len(line) == 0 {
			return fmt.Errorf("route decision log: empty record")
		}
		var entry routeLogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return fmt.Errorf("route decision log: malformed record: %w", err)
		}
		if entry.Timestamp.IsZero() {
			return fmt.Errorf("route decision log: record has no timestamp")
		}
		if err := validateRouteDecision(&entry.Route); err != nil {
			return err
		}
	}
	return nil
}

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
	if err := validateRouteDecision(decision); err != nil {
		return err
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
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("route decision log: open %q: %w", path, err)
	}
	existing, err := io.ReadAll(f)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("route decision log: read %q: %w", path, err)
	}
	if err := validateRouteLog(existing); err != nil {
		_ = f.Close()
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		_ = f.Close()
		return fmt.Errorf("route decision log: seek %q: %w", path, err)
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
