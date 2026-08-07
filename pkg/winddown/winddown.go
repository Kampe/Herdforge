// Package winddown owns the durable wind-down posture authority.
package winddown

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var (
	ErrStateMissing       = errors.New("winddown state is missing")
	ErrStateCorrupt       = errors.New("winddown state is corrupt")
	ErrGenerationInvalid  = errors.New("winddown generation must be positive")
	ErrGenerationStale    = errors.New("winddown generation is stale")
	ErrGenerationConflict = errors.New("winddown generation conflicts with existing state")
	ErrWinddownActive     = errors.New("winddown is active")
	ErrDeadlineExceeded   = errors.New("winddown deadline has passed")
	ErrActorInvalid       = errors.New("winddown actor must be a bounded stable identifier")
	ErrReasonInvalid      = errors.New("winddown reason must be bounded non-whitespace text")
)

const (
	maxEvidenceLength = 128
	maxStateBytes     = 16 * 1024
)

// Clock allows callers and tests to control timestamps without sleeping.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// State is the complete durable posture. Generation is the monotonic fencing token.
type State struct {
	Enabled    bool       `json:"enabled"`
	Actor      string     `json:"actor"`
	Reason     string     `json:"reason"`
	Timestamp  time.Time  `json:"timestamp"`
	Generation uint64     `json:"generation"`
	Deadline   *time.Time `json:"deadline,omitempty"`
}

// Authority serializes updates in-process and persists each accepted update atomically.
type Authority struct {
	path  string
	clock Clock
	local chan struct{}
}

func New(path string, clock Clock) (*Authority, error) {
	if path == "" {
		return nil, errors.New("winddown state path is required")
	}
	if clock == nil {
		clock = realClock{}
	}
	return &Authority{path: path, clock: clock, local: make(chan struct{}, 1)}, nil
}

// Update applies a posture at generation. Equal retries are idempotent; every other
// update must fence the currently durable generation with a strictly larger token.
func (a *Authority) Update(ctx context.Context, enabled bool, actor, reason string, generation uint64, deadline *time.Time) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if generation == 0 {
		return State{}, ErrGenerationInvalid
	}
	if !validActor(actor) {
		return State{}, ErrActorInvalid
	}
	if !validReason(reason) {
		return State{}, ErrReasonInvalid
	}
	release, err := a.acquireLocal(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	unlock, err := a.lock(ctx, unix.LOCK_EX)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return State{}, ctxErr
		}
		return State{}, fmt.Errorf("lock winddown state: %w", err)
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	current, err := a.read()
	if err != nil && !errors.Is(err, ErrStateMissing) {
		return State{}, err
	}
	if err == nil {
		if generation < current.Generation {
			return State{}, ErrGenerationStale
		}
		candidate := State{Enabled: enabled, Actor: actor, Reason: reason, Generation: generation, Deadline: cloneTime(deadline)}
		if generation == current.Generation {
			if equalPosture(current, candidate) {
				return current, nil
			}
			return State{}, ErrGenerationConflict
		}
	}
	state := State{Enabled: enabled, Actor: actor, Reason: reason, Timestamp: a.clock.Now().UTC(), Generation: generation, Deadline: cloneTime(deadline)}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := a.write(state); err != nil {
		return State{}, fmt.Errorf("persist winddown state: %w", err)
	}
	return state, nil
}

// Read returns durable state and fails closed for missing, unreadable, partial, or corrupt data.
func (a *Authority) Read(ctx context.Context) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	release, err := a.acquireLocal(ctx)
	if err != nil {
		return State{}, err
	}
	defer release()
	unlock, err := a.lock(ctx, unix.LOCK_SH)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return State{}, ctxErr
		}
		return State{}, fmt.Errorf("lock winddown state: %w", err)
	}
	defer unlock()
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	return a.read()
}

func (a *Authority) acquireLocal(ctx context.Context) (func(), error) {
	select {
	case a.local <- struct{}{}:
		return func() { <-a.local }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *Authority) lock(ctx context.Context, mode int) (func(), error) {
	f, err := os.OpenFile(a.path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, err
		}
		err := unix.Flock(int(f.Fd()), mode|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			_ = f.Close()
			return nil, err
		}
		timer := time.NewTimer(5 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = f.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() {
		_ = unix.Flock(int(f.Fd()), unix.LOCK_UN)
		_ = f.Close()
	}, nil
}

// Gate permits work only when a valid durable posture explicitly disables wind-down.
// A deadline never auto-disables the posture; it is surfaced as a distinct failure.
func (a *Authority) Gate(ctx context.Context) error {
	state, err := a.Read(ctx)
	if err != nil {
		return err
	}
	if !state.Enabled {
		return nil
	}
	if state.Deadline != nil && !a.clock.Now().Before(*state.Deadline) {
		return ErrDeadlineExceeded
	}
	return ErrWinddownActive
}

// DefaultStatePath is the durable wind-down state file every production
// caller in this repo uses unless HERD_WINDDOWN_STATE overrides it
// (mirrors cmd/herd's own winddownStatePath).
func DefaultStatePath() string {
	if path := strings.TrimSpace(os.Getenv("HERD_WINDDOWN_STATE")); path != "" {
		return path
	}
	return ".herd/winddown.json"
}

// RequireAdmission is the one production posture gate for work that can
// claim or re-engage fleet capacity: missing, corrupt, or unreadable state
// is deliberately rejected, same as cmd/herd's requireFleetAdmission. An
// empty path resolves via DefaultStatePath.
func RequireAdmission(ctx context.Context, path string) error {
	if strings.TrimSpace(path) == "" {
		path = DefaultStatePath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create wind-down state directory: %w", err)
	}
	a, err := New(path, nil)
	if err != nil {
		return err
	}
	return a.Gate(ctx)
}

func (a *Authority) read() (State, error) {
	f, err := os.Open(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, ErrStateMissing
		}
		return State{}, fmt.Errorf("read winddown state: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return State{}, fmt.Errorf("read winddown state: %w", err)
	}
	if len(b) > maxStateBytes {
		return State{}, ErrStateCorrupt
	}
	var wire stateWire
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if len(b) == 0 || dec.Decode(&wire) != nil {
		return State{}, ErrStateCorrupt
	}
	var trailing any
	if dec.Decode(&trailing) != io.EOF {
		return State{}, ErrStateCorrupt
	}
	state, err := wire.state()
	if err != nil {
		return State{}, ErrStateCorrupt
	}
	return state, nil
}

type stateWire struct {
	Enabled    bool    `json:"enabled"`
	Actor      string  `json:"actor"`
	Reason     string  `json:"reason"`
	Timestamp  string  `json:"timestamp"`
	Generation uint64  `json:"generation"`
	Deadline   *string `json:"deadline,omitempty"`
}

func (w stateWire) state() (State, error) {
	if w.Generation == 0 || !validActor(w.Actor) || !validReason(w.Reason) {
		return State{}, ErrStateCorrupt
	}
	timestamp, err := parseCanonicalTime(w.Timestamp)
	if err != nil {
		return State{}, ErrStateCorrupt
	}
	var deadline *time.Time
	if w.Deadline != nil {
		parsed, parseErr := parseCanonicalTime(*w.Deadline)
		if parseErr != nil {
			return State{}, ErrStateCorrupt
		}
		deadline = &parsed
	}
	return State{Enabled: w.Enabled, Actor: w.Actor, Reason: w.Reason, Timestamp: timestamp, Generation: w.Generation, Deadline: deadline}, nil
}

func parseCanonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrStateCorrupt
	}
	return parsed.UTC(), nil
}

func validActor(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxEvidenceLength || filepath.IsAbs(value) {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r == '.' || r == ':' || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return false
		}
	}
	return true
}

func validReason(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > maxEvidenceLength || filepath.IsAbs(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (a *Authority) write(state State) error {
	dir := filepath.Dir(a.path)
	f, err := os.CreateTemp(dir, ".winddown-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		var b []byte
		b, err = json.Marshal(state)
		if err == nil {
			_, err = f.Write(append(b, '\n'))
		}
		if err == nil {
			err = f.Sync()
		}
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, a.path); err != nil {
		return err
	}
	return syncDir(dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := t.UTC()
	return &c
}
func equalPosture(a, b State) bool {
	return a.Enabled == b.Enabled && a.Actor == b.Actor && a.Reason == b.Reason && a.Generation == b.Generation && timesEqual(a.Deadline, b.Deadline)
}
func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}
