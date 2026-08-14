// Package remoteci persists exact-candidate remote-CI settlement evidence.
package remoteci

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

const Version1 = 1

var (
	ErrInvalid     = errors.New("remote-ci: invalid settlement")
	ErrStale       = errors.New("remote-ci: settlement candidate is stale")
	ErrPending     = errors.New("remote-ci: checks are pending")
	ErrNoChecks    = errors.New("remote-ci: no checks reported")
	ErrUnavailable = errors.New("remote-ci: watcher unavailable")
)

type State string

const (
	StatePending State = "pending"
	StatePassed  State = "passed"
	StateFailed  State = "failed"
)

var exactSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// Binding identifies the one immutable candidate a remote-CI result may settle.
type Binding struct {
	Repository     string   `json:"repository"`
	CandidateSHA   string   `json:"candidate_sha"`
	PolicyRevision string   `json:"policy_revision"`
	Attempt        int64    `json:"attempt"`
	RequiredChecks []string `json:"required_checks"`
}

// Settlement is the minimal durable, versioned remote-CI result.
type Settlement struct {
	Version    int     `json:"version"`
	Binding    Binding `json:"binding"`
	State      State   `json:"state"`
	Diagnostic string  `json:"diagnostic,omitempty"`
}

func (b Binding) Validate() error {
	if strings.TrimSpace(b.Repository) == "" || strings.TrimSpace(b.PolicyRevision) == "" || b.Attempt < 1 {
		return fmt.Errorf("%w: repository, policy revision, and positive attempt are required", ErrInvalid)
	}
	if !exactSHA.MatchString(b.CandidateSHA) {
		return fmt.Errorf("%w: candidate must be an exact lowercase immutable SHA", ErrInvalid)
	}
	if len(b.RequiredChecks) == 0 {
		return fmt.Errorf("%w: required checks are required", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, check := range b.RequiredChecks {
		name := strings.TrimSpace(check)
		if name == "" || seen[strings.ToLower(name)] {
			return fmt.Errorf("%w: required checks must be unique and non-empty", ErrInvalid)
		}
		seen[strings.ToLower(name)] = true
	}
	return nil
}

func (s Settlement) Validate() error {
	if s.Version != Version1 {
		return fmt.Errorf("%w: unsupported version %d", ErrInvalid, s.Version)
	}
	if err := s.Binding.Validate(); err != nil {
		return err
	}
	switch s.State {
	case StatePending, StatePassed, StateFailed:
		return nil
	default:
		return fmt.Errorf("%w: unknown state %q", ErrInvalid, s.State)
	}
}

// Settle refuses a result whose candidate differs from the registered watch.
func Settle(watch Binding, settlement Settlement) error {
	if err := watch.Validate(); err != nil {
		return err
	}
	if err := settlement.Validate(); err != nil {
		return err
	}
	if !sameBinding(watch, settlement.Binding) {
		if watch.CandidateSHA != settlement.Binding.CandidateSHA {
			return fmt.Errorf("%w: watch candidate %s does not match settlement candidate %s", ErrStale, watch.CandidateSHA, settlement.Binding.CandidateSHA)
		}
		return fmt.Errorf("%w: settlement binding does not match registered watch", ErrInvalid)
	}
	return nil
}

// Watcher is provider-neutral: each provider independently resolves one exact
// immutable candidate and reports a fail-closed state.
type Watcher interface {
	Watch(context.Context, Binding) (Settlement, error)
}

// Store is an append-only JSONL ledger. A key excludes Attempt deliberately:
// retries of the same candidate under the same policy deduplicate instead of
// creating a second authority record.
type Store struct {
	path string
	mu   sync.Mutex
}

type record struct {
	Kind       string     `json:"kind"`
	Settlement Settlement `json:"settlement"`
}

func Open(path string) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, fmt.Errorf("%w: store path is required", ErrInvalid)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("remote-ci: create store directory: %w", err)
	}
	return &Store{path: path}, nil
}

// Register creates a pending watch once. Existing watches must have the same
// complete binding; differing attempts are refused rather than silently reused.
func (s *Store) Register(binding Binding) (Settlement, bool, error) {
	if s == nil {
		return Settlement{}, false, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := binding.Validate(); err != nil {
		return Settlement{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return Settlement{}, false, err
	}
	key := watchKey(binding)
	for _, prior := range records {
		if watchKey(prior.Binding) != key {
			continue
		}
		if !sameBinding(prior.Binding, binding) {
			return Settlement{}, false, fmt.Errorf("%w: candidate/policy watch already belongs to another binding", ErrInvalid)
		}
		return prior, false, nil
	}
	created := Settlement{Version: Version1, Binding: binding, State: StatePending}
	if err := s.appendLocked(record{Kind: "watch", Settlement: created}); err != nil {
		return Settlement{}, false, err
	}
	return created, true, nil
}

// PersistTerminal records one immutable terminal outcome. It never turns a
// pending watch into a result for a different candidate or policy.
func (s *Store) PersistTerminal(settlement Settlement) (bool, error) {
	if s == nil {
		return false, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := settlement.Validate(); err != nil {
		return false, err
	}
	if settlement.State == StatePending {
		return false, fmt.Errorf("%w: pending is not terminal", ErrInvalid)
	}
	settlement.Diagnostic = redactBounded(settlement.Diagnostic)
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return false, err
	}
	key := watchKey(settlement.Binding)
	found := false
	for _, prior := range records {
		if watchKey(prior.Binding) != key {
			continue
		}
		if !sameBinding(prior.Binding, settlement.Binding) {
			return false, fmt.Errorf("%w: settlement binding does not match registered watch", ErrInvalid)
		}
		found = true
		if prior.State != StatePending {
			if prior.State == settlement.State && prior.Diagnostic == settlement.Diagnostic {
				return false, nil
			}
			return false, fmt.Errorf("%w: terminal settlement already exists", ErrInvalid)
		}
	}
	if !found {
		return false, fmt.Errorf("%w: no registered watch", ErrInvalid)
	}
	if err := s.appendLocked(record{Kind: "settlement", Settlement: settlement}); err != nil {
		return false, err
	}
	return true, nil
}

// Load returns the current durable record for one exact binding.
func (s *Store) Load(binding Binding) (Settlement, error) {
	if s == nil {
		return Settlement{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := binding.Validate(); err != nil {
		return Settlement{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return Settlement{}, err
	}
	key := watchKey(binding)
	for _, prior := range records {
		if watchKey(prior.Binding) != key {
			continue
		}
		if !sameBinding(prior.Binding, binding) {
			return Settlement{}, fmt.Errorf("%w: candidate/policy watch belongs to another binding", ErrInvalid)
		}
		return prior, nil
	}
	return Settlement{}, fmt.Errorf("%w: no registered watch", ErrInvalid)
}

// List returns every current settlement in deterministic binding order. It is
// read-only and deliberately leaves policy interpretation to its caller.
func (s *Store) List() ([]Settlement, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	sort.Slice(records, func(i, j int) bool {
		left, right := records[i].Binding, records[j].Binding
		if left.CandidateSHA != right.CandidateSHA {
			return left.CandidateSHA < right.CandidateSHA
		}
		if left.PolicyRevision != right.PolicyRevision {
			return left.PolicyRevision < right.PolicyRevision
		}
		return left.Attempt < right.Attempt
	})
	return records, nil
}

func (s *Store) readLocked() ([]Settlement, error) {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("remote-ci: read ledger: %w", err)
	}
	defer f.Close()
	byKey := map[string]Settlement{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024), 64*1024)
	line := 0
	for scanner.Scan() {
		line++
		var r record
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil || (r.Kind != "watch" && r.Kind != "settlement") || r.Settlement.Validate() != nil {
			return nil, fmt.Errorf("%w: corrupt ledger line %d", ErrInvalid, line)
		}
		key := watchKey(r.Settlement.Binding)
		if prior, ok := byKey[key]; ok {
			if !sameBinding(prior.Binding, r.Settlement.Binding) {
				return nil, fmt.Errorf("%w: conflicting watch binding", ErrInvalid)
			}
			if prior.State != StatePending && r.Kind == "settlement" {
				return nil, fmt.Errorf("%w: duplicate terminal settlement", ErrInvalid)
			}
		}
		if r.Kind == "watch" {
			if _, ok := byKey[key]; ok {
				return nil, fmt.Errorf("%w: duplicate watch", ErrInvalid)
			}
		}
		byKey[key] = r.Settlement
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("remote-ci: read ledger: %w", err)
	}
	out := make([]Settlement, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	return out, nil
}

func (s *Store) appendLocked(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("remote-ci: append ledger: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("remote-ci: append ledger: %w", err)
	}
	return f.Sync()
}

func watchKey(b Binding) string {
	sum := sha256.Sum256([]byte(b.Repository + "\x00" + b.CandidateSHA + "\x00" + b.PolicyRevision))
	return hex.EncodeToString(sum[:])
}

func sameBinding(left, right Binding) bool {
	if left.Repository != right.Repository || left.CandidateSHA != right.CandidateSHA || left.PolicyRevision != right.PolicyRevision || left.Attempt != right.Attempt || len(left.RequiredChecks) != len(right.RequiredChecks) {
		return false
	}
	for i := range left.RequiredChecks {
		if left.RequiredChecks[i] != right.RequiredChecks[i] {
			return false
		}
	}
	return true
}

// Revision creates an opaque deterministic revision for a policy snapshot.
func Revision(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

const maxDiagnosticBytes = 1024

func redactBounded(in string) string {
	in = strings.TrimSpace(in)
	for _, pattern := range []*regexp.Regexp{
		regexp.MustCompile(`(?i)(authorization|token|password)\s*[:=]\s*[^\s]+`),
		regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._-]+`),
		regexp.MustCompile(`gh[pousr]_[A-Za-z0-9_]+`),
	} {
		in = pattern.ReplaceAllString(in, "[REDACTED]")
	}
	if len(in) > maxDiagnosticBytes {
		return in[:maxDiagnosticBytes] + "…"
	}
	return in
}
