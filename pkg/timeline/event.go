// Package timeline provides the durable, append-only execution timeline.
//
// It deliberately records observations; it does not decide task state. The
// lifecycle and orchestration stores remain authoritative for their own state.
package timeline

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const Version1 = 1

const unknown = "unknown"

var (
	ErrInvalidEnvelope = errors.New("timeline: invalid envelope")
	ErrCorrupt         = errors.New("timeline: corrupt event stream")
)

// CorrelationStatus says whether the binding is established. Unknown and
// blocked correlations must retain the literal unknown values; callers must
// never guess identifiers from raw output or nearby events.
type CorrelationStatus string

const (
	CorrelationKnown   CorrelationStatus = "known"
	CorrelationUnknown CorrelationStatus = "unknown"
	CorrelationBlocked CorrelationStatus = "blocked"
)

// Envelope is the stable, versioned execution-event contract. RawOutputRef is
// intentionally only a reference to non-authoritative diagnostic data; no raw
// process output is accepted as correlation evidence.
type Envelope struct {
	Version      int               `json:"version"`
	ID           string            `json:"id"`
	BuildRun     string            `json:"build_run"`
	Task         string            `json:"task"`
	Attempt      string            `json:"attempt"`
	Lane         string            `json:"lane"`
	Session      string            `json:"session"`
	Model        string            `json:"model"`
	Provider     string            `json:"provider"`
	Source       string            `json:"source"`
	Type         string            `json:"type"`
	Time         time.Time         `json:"time"`
	Evidence     string            `json:"evidence"`
	Correlation  CorrelationStatus `json:"correlation"`
	RawOutputRef string            `json:"raw_output_ref,omitempty"`
}

// Binding is trusted orchestration context supplied by an authority. It is
// never recovered from raw process output.
type Binding struct {
	BuildRun string
	Task     string
	Attempt  string
	Lane     string
	Session  string
	Model    string
	Provider string
}

// LifecycleEvent is the minimal immutable fact copied from lifecycle's own
// authoritative event stream.
type LifecycleEvent struct {
	ID       int64
	ToState  string
	Actor    string
	Evidence string
	Time     time.Time
}

// FromLifecycle creates an observation of an already-authoritative lifecycle
// event. It never changes lifecycle state or treats diagnostic output as proof.
func FromLifecycle(binding Binding, event LifecycleEvent) (Envelope, error) {
	evidence := event.Evidence
	if evidence == "" {
		evidence = fmt.Sprintf("lifecycle-event:%d", event.ID)
	}
	envelope := Envelope{
		Version: Version1, ID: NewID("lifecycle", fmt.Sprintf("%d", event.ID)),
		BuildRun: binding.BuildRun, Task: binding.Task, Attempt: binding.Attempt,
		Lane: binding.Lane, Session: binding.Session, Model: binding.Model,
		Provider: binding.Provider, Source: "lifecycle", Type: "transition." + event.ToState,
		Time: event.Time.UTC(), Evidence: evidence, Correlation: CorrelationKnown,
	}
	if err := envelope.Validate(); err != nil {
		return Envelope{}, fmt.Errorf("timeline lifecycle binding: %w", err)
	}
	return envelope, nil
}

// Validate enforces complete bindings. A source that cannot prove correlation
// records a deliberately unknown or blocked envelope; it cannot emit a partly
// populated "known" envelope that could be misattributed by readers.
func (e Envelope) Validate() error {
	if e.Version != Version1 || strings.TrimSpace(e.ID) == "" || strings.TrimSpace(e.Source) == "" || strings.TrimSpace(e.Type) == "" || e.Time.IsZero() || strings.TrimSpace(e.Evidence) == "" {
		return fmt.Errorf("%w: version, id, source, type, time, and evidence are required", ErrInvalidEnvelope)
	}
	values := []string{e.BuildRun, e.Task, e.Attempt, e.Lane, e.Session, e.Model, e.Provider}
	switch e.Correlation {
	case CorrelationKnown:
		for _, value := range values {
			if strings.TrimSpace(value) == "" || value == unknown {
				return fmt.Errorf("%w: known correlation requires every identity", ErrInvalidEnvelope)
			}
		}
	case CorrelationUnknown, CorrelationBlocked:
		for _, value := range values {
			if value != unknown {
				return fmt.Errorf("%w: %s correlation requires literal unknown identities", ErrInvalidEnvelope, e.Correlation)
			}
		}
	default:
		return fmt.Errorf("%w: correlation must be known, unknown, or blocked", ErrInvalidEnvelope)
	}
	return nil
}

// NewID derives a deterministic content ID for a producer idempotency key.
func NewID(source, idempotencyKey string) string {
	sum := sha256.Sum256([]byte(source + "\x00" + idempotencyKey))
	return "evt_" + hex.EncodeToString(sum[:])
}

type Filter struct {
	BuildRun string
	Task     string
	Lane     string
	Session  string
	Model    string
	Provider string
	Source   string
	Type     string
	After    time.Time
	Before   time.Time
}

func (f Filter) matches(e Envelope) bool {
	return (f.BuildRun == "" || f.BuildRun == e.BuildRun) &&
		(f.Task == "" || f.Task == e.Task) &&
		(f.Lane == "" || f.Lane == e.Lane) &&
		(f.Session == "" || f.Session == e.Session) &&
		(f.Model == "" || f.Model == e.Model) &&
		(f.Provider == "" || f.Provider == e.Provider) &&
		(f.Source == "" || f.Source == e.Source) &&
		(f.Type == "" || f.Type == e.Type) &&
		(f.After.IsZero() || e.Time.After(f.After)) &&
		(f.Before.IsZero() || e.Time.Before(f.Before))
}

// Store is a JSONL append log. O_APPEND makes each event a new record; read
// validates every record before exposing any timeline, so corruption cannot be
// silently turned into an attribution.
type Store struct {
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("timeline: path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("timeline: create directory: %w", err)
	}
	return &Store{path: path}, nil
}

func (s *Store) Append(e Envelope) error {
	if err := e.Validate(); err != nil {
		return err
	}
	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("timeline: marshal envelope: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	existing, err := s.readLocked(Filter{})
	if err != nil {
		return err
	}
	for _, prior := range existing {
		if prior.ID == e.ID {
			if sameEnvelope(prior, e) {
				return nil // retry of an already-durable authoritative observation
			}
			return fmt.Errorf("%w: id %q is already bound to a different event", ErrInvalidEnvelope, e.ID)
		}
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("timeline: open append log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("timeline: append envelope: %w", err)
	}
	return f.Sync()
}

func sameEnvelope(left, right Envelope) bool {
	if !left.Time.Equal(right.Time) {
		return false
	}
	left.Time = time.Time{}
	right.Time = time.Time{}
	return left == right
}

func (s *Store) Read(filter Filter) ([]Envelope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readLocked(filter)
}

func (s *Store) readLocked(filter Filter) ([]Envelope, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return []Envelope{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("timeline: open read log: %w", err)
	}
	defer f.Close()
	return read(f, filter)
}

func read(r io.Reader, filter Filter) ([]Envelope, error) {
	scanner := bufio.NewScanner(r)
	// Evidence may be a signed receipt, so permit a large but bounded record.
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	var events []Envelope
	line := 0
	for scanner.Scan() {
		line++
		if strings.TrimSpace(scanner.Text()) == "" {
			return nil, fmt.Errorf("%w: blank record at line %d", ErrCorrupt, line)
		}
		var event Envelope
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("%w: decode line %d: %v", ErrCorrupt, line, err)
		}
		if err := event.Validate(); err != nil {
			return nil, fmt.Errorf("%w: invalid line %d: %v", ErrCorrupt, line, err)
		}
		if filter.matches(event) {
			events = append(events, event)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: read: %v", ErrCorrupt, err)
	}
	sort.SliceStable(events, func(i, j int) bool { return events[i].Time.Before(events[j].Time) })
	return events, nil
}
