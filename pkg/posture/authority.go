package posture

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

// Mode is the durable provider-family execution policy.
type Mode string

const (
	// ModeClear means no family restriction (routing uses full task-fit).
	ModeClear Mode = "clear"
	// ModeClaudeOnly routes only through the native Claude surface.
	ModeClaudeOnly Mode = "claude-only"
	// ModeNoClaude excludes every Anthropic-family candidate.
	ModeNoClaude Mode = "no-claude"
)

// State is the complete durable family posture. Generation is the fencing token.
type State struct {
	Mode       Mode       `json:"mode"`
	Actor      string     `json:"actor"`
	Reason     string     `json:"reason"`
	Timestamp  time.Time  `json:"timestamp"`
	Generation uint64     `json:"generation"`
	Scope      string     `json:"scope"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

// Expired reports whether an optional expiry has passed as of now.
func (s State) Expired(now time.Time) bool {
	return s.ExpiresAt != nil && !now.Before(*s.ExpiresAt)
}

// EffectiveMode returns the mode that route/resolve/dispatch must enforce.
// Expired state is treated as clear (deterministic, not sticky-after-expiry).
func (s State) EffectiveMode(now time.Time) Mode {
	if s.Mode == "" || s.Mode == ModeClear {
		return ModeClear
	}
	if s.Expired(now) {
		return ModeClear
	}
	return s.Mode
}

var (
	ErrStateMissing       = errors.New("family posture state is missing")
	ErrStateCorrupt       = errors.New("family posture state is corrupt")
	ErrGenerationInvalid  = errors.New("family posture generation must be positive")
	ErrGenerationStale    = errors.New("family posture generation is stale")
	ErrGenerationConflict = errors.New("family posture generation conflicts with existing state")
	ErrModeInvalid        = errors.New("family posture mode must be claude-only, no-claude, or clear")
	ErrActorInvalid       = errors.New("family posture actor must be a bounded stable identifier")
	ErrReasonInvalid      = errors.New("family posture reason must be bounded non-whitespace text")
	ErrScopeInvalid       = errors.New("family posture scope must be a bounded stable identifier")
	ErrUnknownState       = errors.New("family posture state is unknown")
)

const (
	maxEvidenceLength = 128
	maxStateBytes     = 16 * 1024
	defaultScope      = "fleet"
)

// Clock allows tests to control timestamps without sleeping.
type Clock interface{ Now() time.Time }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now().UTC() }

// Authority serializes updates and persists each accepted update atomically.
type Authority struct {
	path  string
	clock Clock
	local chan struct{}
}

// DefaultStatePath is the durable family-posture file. HERD_FAMILY_POSTURE
// overrides the path; otherwise it lives under StateDir().
func DefaultStatePath() string {
	if path := strings.TrimSpace(os.Getenv("HERD_FAMILY_POSTURE")); path != "" {
		return path
	}
	return filepath.Join(StateDir(), "family-posture.json")
}

// New constructs an Authority at path.
func New(path string, clock Clock) (*Authority, error) {
	if path == "" {
		return nil, errors.New("family posture state path is required")
	}
	if clock == nil {
		clock = realClock{}
	}
	return &Authority{path: path, clock: clock, local: make(chan struct{}, 1)}, nil
}

// OpenDefault opens the production Authority under DefaultStatePath().
func OpenDefault() (*Authority, error) {
	path := DefaultStatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create family posture state directory: %w", err)
	}
	return New(path, nil)
}

// Update applies mode at generation. Equal retries are idempotent; every other
// update must fence the currently durable generation with a strictly larger token.
func (a *Authority) Update(ctx context.Context, mode Mode, actor, reason, scope string, generation uint64, expiresAt *time.Time) (State, error) {
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if generation == 0 {
		return State{}, ErrGenerationInvalid
	}
	if !validMode(mode) {
		return State{}, ErrModeInvalid
	}
	if !validActor(actor) {
		return State{}, ErrActorInvalid
	}
	if !validReason(reason) {
		return State{}, ErrReasonInvalid
	}
	if scope == "" {
		scope = defaultScope
	}
	if !validScope(scope) {
		return State{}, ErrScopeInvalid
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
		return State{}, fmt.Errorf("lock family posture state: %w", err)
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
		candidate := State{Mode: mode, Actor: actor, Reason: reason, Generation: generation, Scope: scope, ExpiresAt: cloneTime(expiresAt)}
		if generation == current.Generation {
			if equalPosture(current, candidate) {
				return current, nil
			}
			return State{}, ErrGenerationConflict
		}
	}
	state := State{
		Mode:       mode,
		Actor:      actor,
		Reason:     reason,
		Timestamp:  a.clock.Now().UTC(),
		Generation: generation,
		Scope:      scope,
		ExpiresAt:  cloneTime(expiresAt),
	}
	if err := ctx.Err(); err != nil {
		return State{}, err
	}
	if err := a.write(state); err != nil {
		return State{}, fmt.Errorf("persist family posture state: %w", err)
	}
	// Keep legacy sentinels in sync so older tooling that only stats the files
	// still sees the same effective switch (read path prefers JSON).
	if err := syncLegacySentinels(state.EffectiveMode(a.clock.Now())); err != nil {
		return State{}, err
	}
	return state, nil
}

// Read returns durable state and fails closed for unreadable/partial/corrupt data.
// Missing is a distinct error so callers can treat "never set" as clear.
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
		return State{}, fmt.Errorf("lock family posture state: %w", err)
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

func (a *Authority) read() (State, error) {
	f, err := os.Open(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return State{}, ErrStateMissing
		}
		return State{}, fmt.Errorf("read family posture state: %w", err)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxStateBytes+1))
	if err != nil {
		return State{}, fmt.Errorf("read family posture state: %w", err)
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
	Mode       string  `json:"mode"`
	Actor      string  `json:"actor"`
	Reason     string  `json:"reason"`
	Timestamp  string  `json:"timestamp"`
	Generation uint64  `json:"generation"`
	Scope      string  `json:"scope"`
	ExpiresAt  *string `json:"expires_at,omitempty"`
}

func (w stateWire) state() (State, error) {
	mode := Mode(w.Mode)
	if !validMode(mode) || w.Generation == 0 || !validActor(w.Actor) || !validReason(w.Reason) || !validScope(w.Scope) {
		return State{}, ErrStateCorrupt
	}
	timestamp, err := parseCanonicalTime(w.Timestamp)
	if err != nil {
		return State{}, ErrStateCorrupt
	}
	var expires *time.Time
	if w.ExpiresAt != nil {
		parsed, parseErr := parseCanonicalTime(*w.ExpiresAt)
		if parseErr != nil {
			return State{}, ErrStateCorrupt
		}
		expires = &parsed
	}
	return State{
		Mode:       mode,
		Actor:      w.Actor,
		Reason:     w.Reason,
		Timestamp:  timestamp,
		Generation: w.Generation,
		Scope:      w.Scope,
		ExpiresAt:  expires,
	}, nil
}

func parseCanonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.UTC().Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrStateCorrupt
	}
	return parsed.UTC(), nil
}

func validMode(mode Mode) bool {
	switch mode {
	case ModeClear, ModeClaudeOnly, ModeNoClaude:
		return true
	}
	return false
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

func validScope(value string) bool {
	return validActor(value) // same grammar: stable identifier, no paths
}

func (a *Authority) write(state State) error {
	dir := filepath.Dir(a.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".family-posture-*")
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
	return a.Mode == b.Mode && a.Actor == b.Actor && a.Reason == b.Reason &&
		a.Generation == b.Generation && a.Scope == b.Scope && timesEqual(a.ExpiresAt, b.ExpiresAt)
}

func timesEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func syncLegacySentinels(mode Mode) error {
	// Best-effort mirror for tools that only know the file sentinels.
	// Errors are fatal so we never claim a JSON write succeeded while the
	// visible switch disagrees.
	switch mode {
	case ModeClaudeOnly:
		if err := writeLegacySentinel(ClaudeOnly, true); err != nil {
			return err
		}
		return writeLegacySentinel(NoClaude, false)
	case ModeNoClaude:
		if err := writeLegacySentinel(NoClaude, true); err != nil {
			return err
		}
		return writeLegacySentinel(ClaudeOnly, false)
	default:
		if err := writeLegacySentinel(ClaudeOnly, false); err != nil {
			return err
		}
		return writeLegacySentinel(NoClaude, false)
	}
}

func writeLegacySentinel(n Name, on bool) error {
	path := n.SentinelPath()
	if !on {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	body := fmt.Sprintf("%s enabled %s\n", n, time.Now().UTC().Format(time.RFC3339))
	return os.WriteFile(path, []byte(body), 0o600)
}
