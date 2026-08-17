package winddown

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }
func newAuthority(t *testing.T) (*Authority, *fakeClock, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	c := &fakeClock{now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	a, err := New(path, c)
	if err != nil {
		t.Fatal(err)
	}
	return a, c, path
}
func apply(t *testing.T, a *Authority, enabled bool, gen uint64, deadline *time.Time) State {
	t.Helper()
	s, err := a.Update(context.Background(), enabled, "worker", "maintenance", gen, deadline)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestAuthorityRestartAndIdempotentRetry(t *testing.T) {
	a, c, path := newAuthority(t)
	first := apply(t, a, true, 1, nil)
	retry, err := a.Update(context.Background(), true, "worker", "maintenance", 1, nil)
	if err != nil || retry != first {
		t.Fatalf("exact retry = %#v, %v; want original state", retry, err)
	}
	b, err := New(path, c)
	if err != nil {
		t.Fatal(err)
	}
	got, err := b.Read(context.Background())
	if err != nil || got != first {
		t.Fatalf("restart read = %#v, %v; want %#v", got, err, first)
	}
	apply(t, b, false, 2, nil)
	if err := b.Gate(context.Background()); err != nil {
		t.Fatalf("disabled posture gate = %v", err)
	}
}

func TestAuthorityInitializeMissingCreatesAuditedDisabledState(t *testing.T) {
	a, c, path := newAuthority(t)
	got, err := a.Initialize(context.Background(), "herd-init", "initialized")
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Generation != 1 || got.Actor != "herd-init" || got.Reason != "initialized" {
		t.Fatalf("initialized state = %#v, want disabled generation-one audit evidence", got)
	}
	if got.Timestamp.IsZero() || !got.Timestamp.Equal(c.now) {
		t.Fatalf("initialized timestamp = %v, want %v", got.Timestamp, c.now)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("initialized state was not persisted: %v", err)
	}
}

func TestAuthorityInitializeIsIdempotentAndPreservesValidState(t *testing.T) {
	a, _, _ := newAuthority(t)
	first, err := a.Initialize(context.Background(), "herd-init", "initialized")
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Initialize(context.Background(), "different", "different")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("reinitialization changed valid state: first=%#v second=%#v", first, second)
	}
}

func TestAuthorityInitializeRejectsCorruptState(t *testing.T) {
	a, _, path := newAuthority(t)
	if err := os.WriteFile(path, []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Initialize(context.Background(), "herd-init", "initialized"); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("corrupt initialization error = %v, want %v", err, ErrStateCorrupt)
	}
}

func TestAuthorityInitializeRecoversAfterMissingState(t *testing.T) {
	a, _, path := newAuthority(t)
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("initial read = %v, want %v", err, ErrStateMissing)
	}
	if _, err := a.Initialize(context.Background(), "herd-init", "recovered"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Initialize(context.Background(), "herd-init", "recovered-again"); err != nil {
		t.Fatal(err)
	}
	state, err := a.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if state.Enabled || state.Generation != 1 || state.Reason != "recovered-again" {
		t.Fatalf("recovered state = %#v", state)
	}
}

func TestAuthorityRejectsStaleAndConflictingGenerations(t *testing.T) {
	a, _, _ := newAuthority(t)
	apply(t, a, true, 4, nil)
	if _, err := a.Update(context.Background(), false, "worker", "maintenance", 3, nil); !errors.Is(err, ErrGenerationStale) {
		t.Fatalf("stale error = %v", err)
	}
	if _, err := a.Update(context.Background(), false, "other", "different", 4, nil); !errors.Is(err, ErrGenerationConflict) {
		t.Fatalf("conflict error = %v", err)
	}
	if _, err := a.Update(context.Background(), false, "worker", "maintenance", 0, nil); !errors.Is(err, ErrGenerationInvalid) {
		t.Fatalf("zero error = %v", err)
	}
}

func TestAuthorityRejectsUnboundedOrUnstableEvidenceBeforeWriting(t *testing.T) {
	a, _, path := newAuthority(t)
	absoluteUserPath := filepath.Join(string(filepath.Separator), "Users", "worker")
	absoluteTempPath := filepath.Join(string(filepath.Separator), "tmp", "maintenance")
	cases := []struct {
		name   string
		actor  string
		reason string
		err    error
	}{
		{"empty actor", "", "maintenance", ErrActorInvalid},
		{"whitespace actor", " worker ", "maintenance", ErrActorInvalid},
		{"path actor", absoluteUserPath, "maintenance", ErrActorInvalid},
		{"empty reason", "worker", "", ErrReasonInvalid},
		{"whitespace reason", "worker", " maintenance ", ErrReasonInvalid},
		{"path reason", "worker", absoluteTempPath, ErrReasonInvalid},
		{"long reason", "worker", strings.Repeat("x", maxEvidenceLength+1), ErrReasonInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := a.Update(context.Background(), true, tc.actor, tc.reason, 1, nil); !errors.Is(err, tc.err) {
				t.Fatalf("error = %v, want %v", err, tc.err)
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("invalid update created state: %v", err)
			}
		})
	}
}

func TestAuthorityFailsClosedForMissingCorruptAndPartialState(t *testing.T) {
	a, _, path := newAuthority(t)
	if err := a.Gate(context.Background()); !errors.Is(err, ErrStateMissing) {
		t.Fatalf("missing gate = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"enabled":false}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Gate(context.Background()); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("corrupt gate = %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"enabled":true,"actor":"worker","reason":"x","generation":1`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("partial read = %v", err)
	}
}

func TestAuthorityRejectsOversizedStateBeforeDecode(t *testing.T) {
	a, _, path := newAuthority(t)
	apply(t, a, true, 1, nil)
	valid, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	valid = append(valid, bytes.Repeat([]byte(" "), maxStateBytes)...)
	if err := os.WriteFile(path, valid, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Read(context.Background()); !errors.Is(err, ErrStateCorrupt) {
		t.Fatalf("oversized state read = %v, want %v", err, ErrStateCorrupt)
	}
}

func TestAuthorityRejectsNoncanonicalAndTrailingJSON(t *testing.T) {
	a, c, path := newAuthority(t)
	deadline := c.now.Add(time.Minute)
	apply(t, a, true, 1, &deadline)
	cases := []string{
		`{"enabled":true,"actor":"worker","reason":"maintenance","timestamp":"2026-08-04T12:00:00Z","generation":1,"extra":false}`,
		`{"enabled":true,"actor":"worker","reason":"maintenance","timestamp":"2026-08-04T12:00:00Z","generation":1} {}`,
		`{"enabled":true,"actor":"worker","reason":"maintenance","timestamp":"2026-08-04T12:00:00+00:00","generation":1}`,
		`{"enabled":true,"actor":"worker","reason":"maintenance","timestamp":"2026-08-04T12:00:00Z","generation":1.0}`,
		`{"enabled":true,"actor":"worker bad","reason":"maintenance","timestamp":"2026-08-04T12:00:00Z","generation":1}`,
		`{"enabled":true,"actor":"worker","reason":"maintenance","timestamp":"2026-08-04T12:00:00Z","generation":1,"deadline":"2026-08-04T13:00:00+00:00"}`,
	}
	for i, raw := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := a.Read(context.Background()); !errors.Is(err, ErrStateCorrupt) {
				t.Fatalf("read = %v, want %v", err, ErrStateCorrupt)
			}
		})
	}
}

func TestAuthorityDeadlineAndCancellation(t *testing.T) {
	a, c, _ := newAuthority(t)
	deadline := c.now.Add(time.Minute)
	apply(t, a, true, 1, &deadline)
	if err := a.Gate(context.Background()); !errors.Is(err, ErrWinddownActive) {
		t.Fatalf("active gate = %v", err)
	}
	c.now = deadline
	if err := a.Gate(context.Background()); !errors.Is(err, ErrDeadlineExceeded) {
		t.Fatalf("expired gate = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read = %v", err)
	}
	if _, err := a.Update(ctx, false, "worker", "x", 2, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled update = %v", err)
	}
}

func TestAuthorityConcurrentExactRetry(t *testing.T) {
	a, _, _ := newAuthority(t)
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := a.Update(context.Background(), true, "worker", "maintenance", 1, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent retry = %v", err)
		}
	}
	if got, err := a.Read(context.Background()); err != nil || got.Generation != 1 || !got.Enabled {
		t.Fatalf("final state = %#v, %v", got, err)
	}
}

func TestAuthorityConcurrentExactRetryAcrossInstances(t *testing.T) {
	a, c, path := newAuthority(t)
	b, err := New(path, c)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, authority := range []*Authority{a, b} {
		wg.Add(1)
		go func(authority *Authority) {
			defer wg.Done()
			_, updateErr := authority.Update(context.Background(), true, "worker", "maintenance", 1, nil)
			errs <- updateErr
		}(authority)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("cross-instance retry = %v", err)
		}
	}
}

func TestAuthorityCancelledUpdateWhileHeldLockPreservesBytes(t *testing.T) {
	a, _, path := newAuthority(t)
	apply(t, a, true, 1, nil)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lockFile, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := a.Update(ctx, false, "worker", "maintenance", 2, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("held-lock update = %v, want deadline", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("cancelled update changed durable bytes: before %q after %q", before, after)
	}
}

func TestAuthorityContextAwareLocalLockPreservesBytes(t *testing.T) {
	a, _, path := newAuthority(t)
	apply(t, a, true, 1, nil)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	a.local <- struct{}{}
	defer func() { <-a.local }()
	for _, operation := range []string{"read", "update"} {
		t.Run(operation, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
			defer cancel()
			started := time.Now()
			if operation == "read" {
				if _, err := a.Read(ctx); !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("held-local read = %v, want deadline", err)
				}
			} else if _, err := a.Update(ctx, false, "worker", "maintenance", 2, nil); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("held-local update = %v, want deadline", err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("held-local %s exceeded bound: %s", operation, elapsed)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !bytes.Equal(after, before) {
				t.Fatalf("held-local %s changed durable bytes", operation)
			}
		})
	}
}
