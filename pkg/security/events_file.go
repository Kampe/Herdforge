package security

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// FileEventSink durably appends security events as JSONL and fsyncs.
// All I/O errors are returned — silent drops are forbidden (FAC-133 audit).
type FileEventSink struct {
	Path string
	mu   sync.Mutex
}

// DefaultEventLogPath is the repo-relative durable denial log (production).
// Tests MUST pass an explicit path under t.TempDir().
const DefaultEventLogPath = ".herd/security-events.jsonl"

// NewFileEventSink opens (or creates) a durable event log.
func NewFileEventSink(path string) *FileEventSink {
	if path == "" {
		if v := os.Getenv("HERD_SECURITY_EVENT_LOG"); v != "" {
			path = v
		} else {
			path = DefaultEventLogPath
		}
	}
	return &FileEventSink{Path: path}
}

// Record appends one JSONL event and syncs the file. Errors propagate.
func (f *FileEventSink) Record(ev SecurityEvent) error {
	if f == nil {
		return fmt.Errorf("%w: nil file event sink", ErrUnknownPolicy)
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	path := f.Path
	if path == "" {
		return fmt.Errorf("%w: empty event log path", ErrUnknownPolicy)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("security event mkdir: %w", err)
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("security event marshal: %w", err)
	}
	b = append(b, '\n')
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("security event open: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(b); err != nil {
		return fmt.Errorf("security event write: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("security event fsync: %w", err)
	}
	return nil
}

// MultiSink fans out to multiple sinks; any error fails the record.
type MultiSink struct {
	Sinks []EventSink
}

// Record fans out; returns the first error.
func (m *MultiSink) Record(ev SecurityEvent) error {
	if m == nil {
		return fmt.Errorf("%w: nil multi sink", ErrUnknownPolicy)
	}
	var first error
	for _, s := range m.Sinks {
		if s == nil {
			if first == nil {
				first = fmt.Errorf("%w: nil sink in multi", ErrUnknownPolicy)
			}
			continue
		}
		if err := s.Record(ev); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// BindDurableEvents attaches a FileEventSink (and optional memory) to policy.
// logPath must be non-empty for production; tests pass t.TempDir paths.
func BindDurableEvents(policy *LaunchPolicy, logPath string, memory *MemorySink) error {
	if policy == nil {
		return ErrUnknownPolicy
	}
	if logPath == "" {
		return fmt.Errorf("%w: durable event log path required (use t.TempDir in tests)", ErrUnknownPolicy)
	}
	file := NewFileEventSink(logPath)
	if memory != nil {
		policy.Events = &MultiSink{Sinks: []EventSink{memory, file}}
	} else if policy.Events != nil {
		policy.Events = &MultiSink{Sinks: []EventSink{policy.Events, file}}
	} else {
		policy.Events = file
	}
	return nil
}

// EnsureObservableEvents rejects nil sinks.
func EnsureObservableEvents(policy *LaunchPolicy) error {
	if policy == nil {
		return ErrUnknownPolicy
	}
	if policy.Events == nil {
		return fmt.Errorf("%w: event sink required (observability must not drop)", ErrUnknownPolicy)
	}
	return nil
}
