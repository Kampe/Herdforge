package posture

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func newAuthority(t *testing.T) (*Authority, *fakeClock, string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERD_STATE_DIR", dir)
	t.Setenv("HERD_FAMILY_POSTURE", "")
	t.Setenv("HERD_CLAUDE_ONLY", "")
	t.Setenv("HERD_NO_CLAUDE", "")
	clock := &fakeClock{now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)}
	path := filepath.Join(dir, "family-posture.json")
	a, err := New(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	return a, clock, path
}

func apply(t *testing.T, a *Authority, mode Mode, gen uint64, expires *time.Time) State {
	t.Helper()
	st, err := a.Update(context.Background(), mode, "operator", "test-reason", "fleet", gen, expires)
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	return st
}

func TestAuthorityRestartAndIdempotentRetry(t *testing.T) {
	a, _, path := newAuthority(t)
	st := apply(t, a, ModeClaudeOnly, 1, nil)
	if st.Mode != ModeClaudeOnly || st.Generation != 1 {
		t.Fatalf("state = %+v", st)
	}
	// Exact retry is idempotent.
	st2, err := a.Update(context.Background(), ModeClaudeOnly, "operator", "test-reason", "fleet", 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if st2.Generation != 1 {
		t.Fatalf("retry bumped generation: %+v", st2)
	}
	// Fresh authority at same path sees the same bytes.
	a2, err := New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := a2.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != ModeClaudeOnly || got.Generation != 1 {
		t.Fatalf("restart read = %+v", got)
	}
}

func TestAuthorityRejectsStaleAndConflictingGenerations(t *testing.T) {
	a, _, _ := newAuthority(t)
	apply(t, a, ModeNoClaude, 2, nil)
	if _, err := a.Update(context.Background(), ModeClear, "operator", "stale", "fleet", 1, nil); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale: %v", err)
	}
	if _, err := a.Update(context.Background(), ModeClaudeOnly, "operator", "conflict", "fleet", 2, nil); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("conflict: %v", err)
	}
}

func TestAuthorityRejectsUnboundedEvidence(t *testing.T) {
	a, _, _ := newAuthority(t)
	// Construct absolute paths at runtime so the source tree never embeds one
	// (preflight/selftest refuse absolute path literals in worktree files).
	absoluteUserPath := filepath.Join(string(filepath.Separator), "Users", "worker")
	cases := []struct {
		name   string
		actor  string
		reason string
		want   error
	}{
		{"empty actor", "", "maintenance", ErrActorInvalid},
		{"empty reason", "worker", "", ErrReasonInvalid},
		{"long reason", "worker", strings.Repeat("x", maxEvidenceLength+1), ErrReasonInvalid},
		{"path actor", absoluteUserPath, "maintenance", ErrActorInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := a.Update(context.Background(), ModeClaudeOnly, tc.actor, tc.reason, "fleet", 1, nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v want %v", err, tc.want)
			}
		})
	}
}

func TestAuthorityFailsClosedForCorruptState(t *testing.T) {
	a, _, path := newAuthority(t)
	if err := os.WriteFile(path, []byte(`{"mode":"claude-only","actor":"x"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
	// Effective must fail closed so routing cannot ignore corrupt state.
	if _, _, err := Effective(context.Background()); err == nil {
		t.Fatal("Effective must fail closed on corrupt durable state")
	}
}

func TestAuthorityExpiryIsDeterministic(t *testing.T) {
	a, _, _ := newAuthority(t)
	// Use wall clock so Effective (which cannot see Authority's test clock)
	// evaluates the same instant.
	future := time.Now().UTC().Add(2 * time.Hour)
	// Canonical RFC3339Nano: re-parse so wire round-trip matches exactly.
	future, err := time.Parse(time.RFC3339Nano, future.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	st := apply(t, a, ModeClaudeOnly, 1, &future)
	if st.Expired(time.Now().UTC()) {
		t.Fatal("future expiry must not be expired yet")
	}
	mode, _, err := Effective(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeClaudeOnly {
		t.Fatalf("before expiry mode=%s", mode)
	}
	past := time.Now().UTC().Add(-time.Minute)
	past, err = time.Parse(time.RFC3339Nano, past.Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	st, err = a.Update(context.Background(), ModeClaudeOnly, "operator", "expired-test", "fleet", 2, &past)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Expired(time.Now().UTC()) {
		t.Fatal("state should be expired")
	}
	mode, _, err = Effective(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeClear {
		t.Fatalf("expired posture must effective-clear, got %s", mode)
	}
}

func TestEffectiveEnvOverridesDurable(t *testing.T) {
	a, _, _ := newAuthority(t)
	apply(t, a, ModeClaudeOnly, 1, nil)
	t.Setenv("HERD_NO_CLAUDE", "1")
	// Contradictory if both env force on — set only no-claude.
	t.Setenv("HERD_CLAUDE_ONLY", "")
	mode, _, err := Effective(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if mode != ModeNoClaude {
		t.Fatalf("env no-claude must win over durable claude-only, got %s", mode)
	}
}

func TestEffectiveContradictoryEnvFailsClosed(t *testing.T) {
	newAuthority(t)
	t.Setenv("HERD_CLAUDE_ONLY", "1")
	t.Setenv("HERD_NO_CLAUDE", "1")
	var contradiction *ErrContradictory
	if _, _, err := Effective(context.Background()); !errors.As(err, &contradiction) {
		t.Fatalf("want contradiction, got %v", err)
	}
}
