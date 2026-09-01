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
	"time"
)

const (
	Version1          = 1
	DefaultLedgerPath = ".herd/remote-ci.jsonl"
)

var (
	ErrInvalid     = errors.New("remote-ci: invalid settlement")
	ErrStale       = errors.New("remote-ci: settlement candidate is stale")
	ErrPending     = errors.New("remote-ci: checks are pending")
	ErrNoChecks    = errors.New("remote-ci: no checks reported")
	ErrUnavailable = errors.New("remote-ci: watcher unavailable")
	ErrAmbiguous   = errors.New("remote-ci: provider state is ambiguous")
	ErrLockTimeout = errors.New("remote-ci: ledger lock timeout")
)

const (
	defaultLockTimeout      = 5 * time.Second
	ledgerLockRetryInterval = 5 * time.Millisecond
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

// NewBinding constructs the one canonical remote-CI identity used by both the
// settlement producer and merge admission. The caller supplies the live merge
// policy revision; this function binds it to the normalized required-check set.
func NewBinding(repository, candidateSHA, mergePolicyRevision string, attempt int64, requiredChecks []string) (Binding, error) {
	checks := normalizeChecks(requiredChecks)
	binding := Binding{
		Repository:     strings.TrimSpace(repository),
		CandidateSHA:   strings.TrimSpace(candidateSHA),
		PolicyRevision: Revision(strings.TrimSpace(mergePolicyRevision), strings.Join(checks, "\x00")),
		Attempt:        attempt,
		RequiredChecks: checks,
	}
	if err := binding.Validate(); err != nil {
		return Binding{}, err
	}
	return binding, nil
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
	path        string
	mu          sync.Mutex
	lockTimeout time.Duration
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
	return &Store{path: path, lockTimeout: defaultLockTimeout}, nil
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
	var result Settlement
	var created bool
	err := s.withExclusiveLock(func() error {
		records, err := s.readLocked()
		if err != nil {
			return err
		}
		key := watchKey(binding)
		for _, prior := range records {
			if watchKey(prior.Binding) != key {
				continue
			}
			if !sameBinding(prior.Binding, binding) {
				return fmt.Errorf("%w: candidate/policy watch already belongs to another binding", ErrInvalid)
			}
			result = prior
			return nil
		}
		result = Settlement{Version: Version1, Binding: binding, State: StatePending}
		if err := s.appendLocked(record{Kind: "watch", Settlement: result}); err != nil {
			return err
		}
		created = true
		return nil
	})
	return result, created, err
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
	written := false
	err := s.withExclusiveLock(func() error {
		records, err := s.readLocked()
		if err != nil {
			return err
		}
		key := watchKey(settlement.Binding)
		found := false
		for _, prior := range records {
			if watchKey(prior.Binding) != key {
				continue
			}
			if !sameBinding(prior.Binding, settlement.Binding) {
				return fmt.Errorf("%w: settlement binding does not match registered watch", ErrInvalid)
			}
			found = true
			if prior.State != StatePending {
				if prior.State == settlement.State && prior.Diagnostic == settlement.Diagnostic {
					return nil
				}
				return fmt.Errorf("%w: terminal settlement already exists", ErrInvalid)
			}
		}
		if !found {
			return fmt.Errorf("%w: no registered watch", ErrInvalid)
		}
		if err := s.appendLocked(record{Kind: "settlement", Settlement: settlement}); err != nil {
			return err
		}
		written = true
		return nil
	})
	return written, err
}

// Load returns the current durable record for one exact binding.
func (s *Store) Load(binding Binding) (Settlement, error) {
	if s == nil {
		return Settlement{}, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	if err := binding.Validate(); err != nil {
		return Settlement{}, err
	}
	var result Settlement
	err := s.withExclusiveLock(func() error {
		records, err := s.readLocked()
		if err != nil {
			return err
		}
		key := watchKey(binding)
		for _, prior := range records {
			if watchKey(prior.Binding) != key {
				continue
			}
			if !sameBinding(prior.Binding, binding) {
				return fmt.Errorf("%w: candidate/policy watch belongs to another binding", ErrInvalid)
			}
			result = prior
			return nil
		}
		return fmt.Errorf("%w: no registered watch", ErrInvalid)
	})
	return result, err
}

// List returns every current settlement in deterministic binding order. It is
// read-only and deliberately leaves policy interpretation to its caller.
func (s *Store) List() ([]Settlement, error) {
	if s == nil {
		return nil, fmt.Errorf("%w: nil store", ErrInvalid)
	}
	var records []Settlement
	err := s.withExclusiveLock(func() error {
		var err error
		records, err = s.readLocked()
		return err
	})
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

func (s *Store) withExclusiveLock(fn func() error) (retErr error) {
	timeout := s.lockTimeout
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := acquireLedgerProcessLock(ctx, &s.mu); err != nil {
		return fmt.Errorf("%w after %s: %v", ErrLockTimeout, timeout, err)
	}
	defer s.mu.Unlock()
	lock, err := os.OpenFile(s.path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("remote-ci: open ledger lock: %w", err)
	}
	locked := false
	defer func() {
		var unlockErr error
		if locked {
			unlockErr = releaseLedgerFileLock(lock)
		}
		closeErr := lock.Close()
		if unlockErr != nil {
			unlockErr = fmt.Errorf("remote-ci: unlock ledger: %w", unlockErr)
		}
		if closeErr != nil {
			closeErr = fmt.Errorf("remote-ci: close ledger lock: %w", closeErr)
		}
		retErr = errors.Join(retErr, unlockErr, closeErr)
	}()
	if err := acquireLedgerFileLock(ctx, lock); err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("%w after %s: %v", ErrLockTimeout, timeout, err)
		}
		return fmt.Errorf("remote-ci: lock ledger: %w", err)
	}
	locked = true
	return fn()
}

func acquireLedgerProcessLock(ctx context.Context, mu *sync.Mutex) error {
	for {
		if mu.TryLock() {
			return nil
		}
		timer := time.NewTimer(ledgerLockRetryInterval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
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
	prior, err := os.ReadFile(s.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remote-ci: stage ledger: %w", err)
	}
	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, ".remote-ci-*.tmp")
	if err != nil {
		return fmt.Errorf("remote-ci: create ledger transaction: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("remote-ci: chmod ledger transaction: %w", err)
	}
	if len(prior) > 0 {
		if _, err := tmp.Write(prior); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("remote-ci: copy ledger transaction: %w", err)
		}
		if prior[len(prior)-1] != '\n' {
			if _, err := tmp.Write([]byte{'\n'}); err != nil {
				_ = tmp.Close()
				return fmt.Errorf("remote-ci: delimit ledger transaction: %w", err)
			}
		}
	}
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("remote-ci: append ledger transaction: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("remote-ci: sync ledger transaction: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("remote-ci: close ledger transaction: %w", err)
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return fmt.Errorf("remote-ci: commit ledger transaction: %w", err)
	}
	dirHandle, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("remote-ci: open ledger directory: %w", err)
	}
	syncErr := dirHandle.Sync()
	closeErr := dirHandle.Close()
	if syncErr != nil || closeErr != nil {
		return errors.Join(syncErr, closeErr)
	}
	return nil
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

func normalizeChecks(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, raw := range in {
		name := strings.TrimSpace(raw)
		key := strings.ToLower(name)
		if name == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
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

// SanitizeDiagnostic returns the bounded redacted form safe for durable or
// machine-readable operator diagnostics.
func SanitizeDiagnostic(in string) string { return redactBounded(in) }
