package envelope

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// DurableIssuerStore persists monotonic lastSeq per (worker,task) with a
// cross-process file lock, unique temp, and fsync (FAC-133 re-admission).
type DurableIssuerStore struct {
	path string
	mu   sync.Mutex // in-process; flock for cross-process
}

// NewDurableIssuerStore opens/creates a JSON state file under path.
func NewDurableIssuerStore(path string) (*DurableIssuerStore, error) {
	if path == "" {
		return nil, fmt.Errorf("%w: durable issuer path required", ErrMissingFields)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &DurableIssuerStore{path: path}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := s.saveLocked(map[string]uint64{}); err != nil {
			return nil, err
		}
	}
	return s, nil
}

func (s *DurableIssuerStore) withFileLock(fn func() error) error {
	lockPath := s.path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	// Exclusive lock; fail closed if cannot acquire within timeout.
	deadline := time.Now().Add(5 * time.Second)
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) //nolint:errcheck
			return fn()
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: durable issuer flock timeout", ErrConflict)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *DurableIssuerStore) loadLocked() (map[string]uint64, error) {
	b, err := os.ReadFile(s.path)
	if err != nil {
		return nil, err
	}
	var m map[string]uint64
	if err := json.Unmarshal(b, &m); err != nil {
		// Corrupt state is fail-closed — never reset to empty (FAC-133).
		return nil, fmt.Errorf("%w: durable issuer corrupt: %v", ErrNotControl, err)
	}
	if m == nil {
		m = map[string]uint64{}
	}
	return m, nil
}

func (s *DurableIssuerStore) saveLocked(m map[string]uint64) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", s.path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Best-effort dir fsync for durability on rename.
	if d, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// NextSeq returns the next monotonic sequence for worker+task and persists it.
func (s *DurableIssuerStore) NextSeq(worker, task string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var seq uint64
	err := s.withFileLock(func() error {
		m, err := s.loadLocked()
		if err != nil {
			return err
		}
		key := worker + "\x00" + task
		m[key]++
		seq = m[key]
		return s.saveLocked(m)
	})
	return seq, err
}

// DurableSessionState is persisted worker control state.
type DurableSessionState struct {
	WorkerSession   string          `json:"worker_session"`
	Task            string          `json:"task"`
	LeaseGeneration int64           `json:"lease_generation"`
	LastSeq         uint64          `json:"last_seq"`
	LastAppliedID   string          `json:"last_applied_id"`
	State           SessionState    `json:"state"`
	BlockReason     string          `json:"block_reason,omitempty"`
	SeenIDs         map[string]bool `json:"seen_ids,omitempty"`
	SeenNonces      map[string]bool `json:"seen_nonces,omitempty"`
	Scope           *Scope          `json:"scope,omitempty"`
	ExpectedIssuer  string          `json:"expected_issuer_session,omitempty"`
}

// SessionStatePath returns durable path for a worker control session.
func SessionStatePath(root, workerSession, task string) string {
	safeW := sanitize(workerSession)
	safeT := sanitize(task)
	return filepath.Join(root, ".herd", "control", "sessions", safeW+"__"+safeT+".json")
}

func sanitize(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "x"
	}
	return string(out)
}

// LoadDurableSession loads session state. Missing file → fresh session.
// Corrupt JSON or identity mismatch against cfg is fail-closed.
func LoadDurableSession(path string, cfg SessionConfig) (*Session, *DurableSessionState, error) {
	sess, err := NewSession(cfg)
	if err != nil {
		return nil, nil, err
	}
	st := &DurableSessionState{
		WorkerSession:   cfg.WorkerSession,
		Task:            cfg.Task,
		LeaseGeneration: cfg.LeaseGeneration,
		State:           StateActive,
		SeenIDs:         map[string]bool{},
		SeenNonces:      map[string]bool{},
		ExpectedIssuer:  cfg.ExpectedIssuerSession,
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return sess, st, nil
		}
		return nil, nil, fmt.Errorf("%w: durable session read: %v", ErrMissingFields, err)
	}
	if jerr := json.Unmarshal(b, st); jerr != nil {
		// Corrupt: fail closed — never treat as fresh active (FAC-133).
		return nil, nil, fmt.Errorf("%w: durable session corrupt: %v", ErrNotControl, jerr)
	}
	// Identity must match cfg — silent rebind would erase security bindings.
	if st.WorkerSession != "" && st.WorkerSession != cfg.WorkerSession {
		return nil, nil, fmt.Errorf("%w: durable session worker mismatch %q != %q", ErrWorkerMismatch, st.WorkerSession, cfg.WorkerSession)
	}
	if st.Task != "" && st.Task != cfg.Task {
		return nil, nil, fmt.Errorf("%w: durable session task mismatch %q != %q", ErrTaskMismatch, st.Task, cfg.Task)
	}
	if st.LeaseGeneration != 0 && st.LeaseGeneration != cfg.LeaseGeneration {
		// Stored lease differs: do not silently adopt — operator must Rebind.
		return nil, nil, fmt.Errorf("%w: durable session lease %d != cfg %d", ErrStaleGeneration, st.LeaseGeneration, cfg.LeaseGeneration)
	}
	sess.mu.Lock()
	sess.lastSeq = st.LastSeq
	sess.lastAppliedID = st.LastAppliedID
	sess.state = st.State
	sess.blockReason = st.BlockReason
	sess.scope = CloneScope(st.Scope)
	if st.LeaseGeneration != 0 {
		sess.leaseGeneration = st.LeaseGeneration
	}
	if st.SeenIDs != nil {
		for id := range st.SeenIDs {
			sess.seenIDs[id] = struct{}{}
		}
	}
	if st.SeenNonces != nil {
		for n := range st.SeenNonces {
			sess.seenNonces[n] = struct{}{}
		}
	}
	if st.ExpectedIssuer != "" {
		if cfg.ExpectedIssuerSession != "" && st.ExpectedIssuer != cfg.ExpectedIssuerSession {
			sess.mu.Unlock()
			return nil, nil, fmt.Errorf("%w: durable expected issuer mismatch", ErrUnauthorizedIssuer)
		}
		sess.expectedIssuer = st.ExpectedIssuer
	}
	sess.mu.Unlock()
	return sess, st, nil
}

// WithSessionFileLock holds an exclusive cross-process lock on the session
// state file for the duration of fn (load/apply/save transaction).
func WithSessionFileLock(path string, fn func() error) error {
	if path == "" {
		return fmt.Errorf("%w: session path", ErrMissingFields)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%w: session flock timeout", ErrConflict)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() {
		_ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	}()
	return fn()
}

// SaveDurableSession persists session after apply/block with flock + fsync.
// When the caller already holds WithSessionFileLock, use SaveDurableSessionLocked.
func SaveDurableSession(path string, sess *Session) error {
	return WithSessionFileLock(path, func() error {
		return SaveDurableSessionLocked(path, sess)
	})
}

// SaveDurableSessionLocked writes session state; caller must hold the session lock.
func SaveDurableSessionLocked(path string, sess *Session) error {
	if sess == nil || path == "" {
		return fmt.Errorf("%w: session/path", ErrMissingFields)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	sess.mu.Lock()
	st := DurableSessionState{
		WorkerSession:   sess.workerSession,
		Task:            sess.task,
		LeaseGeneration: sess.leaseGeneration,
		LastSeq:         sess.lastSeq,
		LastAppliedID:   sess.lastAppliedID,
		State:           sess.state,
		BlockReason:     sess.blockReason,
		Scope:           CloneScope(sess.scope),
		SeenIDs:         map[string]bool{},
		SeenNonces:      map[string]bool{},
		ExpectedIssuer:  sess.expectedIssuer,
	}
	for id := range sess.seenIDs {
		st.SeenIDs[id] = true
	}
	for n := range sess.seenNonces {
		st.SeenNonces[n] = true
	}
	sess.mu.Unlock()

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if serr := d.Sync(); serr != nil {
			_ = d.Close()
			return serr
		}
		_ = d.Close()
	}
	return nil
}
