package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/winddown"
)

type namedLaunchEffects struct {
	provider, claim, worktree, tab, process, prompt int
}

func (e *namedLaunchEffects) run(_ *router.LaunchDecision) error {
	e.provider++
	e.claim++
	e.worktree++
	e.tab++
	e.process++
	e.prompt++
	return nil
}

func writeWinddownPosture(t *testing.T, enabled bool) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "winddown.json")
	t.Setenv("HERD_WINDDOWN_STATE", path)
	a, err := winddown.New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(context.Background(), enabled, "test", "focused-test", 1, nil); err != nil {
		t.Fatal(err)
	}
}

func TestLiveLaunchLifecycleFailsClosedBeforeNamedEffects(t *testing.T) {
	cases := []struct {
		name    string
		prepare func(*testing.T)
		want    error
	}{
		{name: "missing", prepare: func(t *testing.T) { t.Setenv("HERD_WINDDOWN_STATE", filepath.Join(t.TempDir(), "missing.json")) }, want: winddown.ErrStateMissing},
		{name: "corrupt", prepare: func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "corrupt.json")
			t.Setenv("HERD_WINDDOWN_STATE", path)
			if err := os.WriteFile(path, []byte(`{"enabled":false}`), 0600); err != nil {
				t.Fatal(err)
			}
		}, want: winddown.ErrStateCorrupt},
		{name: "enabled", prepare: func(t *testing.T) { writeWinddownPosture(t, true) }, want: winddown.ErrWinddownActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.prepare(t)
			effects := &namedLaunchEffects{}
			err := (liveLaunchLifecycle{}).Run(nil, effects.run)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if *effects != (namedLaunchEffects{}) {
				t.Fatalf("rejected launch reached downstream effects: %+v", effects)
			}
		})
	}
}

func TestLiveLaunchLifecycleAllowsDisabledPosture(t *testing.T) {
	writeWinddownPosture(t, false)
	effects := &namedLaunchEffects{}
	if err := (liveLaunchLifecycle{}).Run(nil, effects.run); err != nil {
		t.Fatal(err)
	}
	if effects.provider != 1 || effects.claim != 1 || effects.worktree != 1 || effects.tab != 1 || effects.process != 1 || effects.prompt != 1 {
		t.Fatalf("disabled posture did not reach each named effect exactly once: %+v", effects)
	}
}

func TestLiveLaunchLifecycleHonorsDeadline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "winddown.json")
	t.Setenv("HERD_WINDDOWN_STATE", path)
	clock := &testWinddownClock{now: time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)}
	a, err := winddown.New(path, clock)
	if err != nil {
		t.Fatal(err)
	}
	deadline := clock.now.Add(time.Minute)
	if _, err := a.Update(context.Background(), true, "test", "deadline-test", 1, &deadline); err != nil {
		t.Fatal(err)
	}
	clock.now = deadline
	effects := &namedLaunchEffects{}
	err = (liveLaunchLifecycle{}).Run(nil, effects.run)
	if !errors.Is(err, winddown.ErrDeadlineExceeded) {
		t.Fatalf("error = %v, want %v", err, winddown.ErrDeadlineExceeded)
	}
	if *effects != (namedLaunchEffects{}) {
		t.Fatalf("expired posture reached downstream effects: %+v", effects)
	}
}

type testWinddownClock struct{ now time.Time }

func (c *testWinddownClock) Now() time.Time { return c.now }
