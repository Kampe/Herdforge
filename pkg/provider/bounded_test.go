package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// FAC-607: the incident was rc=124 with ZERO stdout across herd deps check,
// board-sync, board-audit and herd lost. These pin the properties that make
// that impossible, in the terms the incident asked for: provider, phase,
// deadline, last successful cache revision.

func TestTimeoutReportsEverythingAnOperatorAskedFor(t *testing.T) {
	_, diag, err := BoundedRead(context.Background(), "kaneo", 40*time.Millisecond, "rev-8811e89",
		func(ctx context.Context, p *Phases) (int, error) {
			p.Enter("paginating board census")
			<-ctx.Done()
			return 0, ctx.Err()
		})

	if err == nil {
		t.Fatal("a read that ran out of budget returned success")
	}
	if diag.Outcome != ReadTimedOut {
		t.Fatalf("outcome = %q, want timed-out", diag.Outcome)
	}
	for _, want := range []string{"kaneo", "paginating board census", "40ms", "rev-8811e89"} {
		if !strings.Contains(diag.String(), want) {
			t.Fatalf("diagnostics omit %q, so an operator cannot act on them:\n%s", want, diag.String())
		}
	}
}

// The property the whole card turns on.
func TestATimedOutReadIsUnknownAndNotEmpty(t *testing.T) {
	_, timedOut, _ := BoundedRead(context.Background(), "kaneo", 20*time.Millisecond, "",
		func(ctx context.Context, p *Phases) ([]string, error) {
			p.Enter("listing tasks")
			<-ctx.Done()
			return nil, ctx.Err()
		})

	empty, complete, err := BoundedRead(context.Background(), "kaneo", time.Second, "",
		func(ctx context.Context, p *Phases) ([]string, error) {
			p.Enter("listing tasks")
			return []string{}, nil
		})
	if err != nil {
		t.Fatalf("a genuinely empty read failed: %v", err)
	}

	// Both produced no data. Only one of them means "there is nothing there".
	if !timedOut.Unknown() {
		t.Fatal("a timed-out read did not classify as UNKNOWN, so a consumer may read it as clean")
	}
	if complete.Unknown() {
		t.Fatal("a genuinely empty read classified as UNKNOWN")
	}
	if len(empty) != 0 {
		t.Fatal("fixture returned data")
	}
	if timedOut.Outcome == complete.Outcome {
		t.Fatal("timeout and empty-result share an outcome; collapsing them IS the defect")
	}
}

func TestDiagnosticsAreNeverSilent(t *testing.T) {
	// rc=124 with zero stdout is the reported symptom. There is no path here
	// that yields an empty report.
	cases := map[string]func(context.Context, *Phases) (int, error){
		"timeout": func(ctx context.Context, p *Phases) (int, error) { <-ctx.Done(); return 0, ctx.Err() },
		"failure": func(ctx context.Context, p *Phases) (int, error) { return 0, errors.New("provider refused") },
		"success": func(ctx context.Context, p *Phases) (int, error) { return 7, nil },
	}
	for name, fn := range cases {
		_, diag, _ := BoundedRead(context.Background(), "kaneo", 30*time.Millisecond, "rev-1", fn)
		if strings.TrimSpace(diag.String()) == "" {
			t.Fatalf("%s produced an empty diagnostic line", name)
		}
		if !strings.Contains(diag.String(), `"provider":"kaneo"`) {
			t.Fatalf("%s diagnostics do not name the provider: %s", name, diag.String())
		}
	}
}

func TestFailedReadPreservesPartialResultAndTypedCause(t *testing.T) {
	typedCause := errors.New("semantic rejection")
	got, diag, err := BoundedRead(context.Background(), "kaneo", time.Second, "",
		func(context.Context, *Phases) (int, error) {
			return 7, typedCause
		})

	if got != 7 {
		t.Fatalf("result = %d, want populated callback result 7", got)
	}
	if !errors.Is(err, typedCause) {
		t.Fatalf("error %v does not preserve typed callback cause", err)
	}
	if !diag.Unknown() || diag.Outcome != ReadFailed {
		t.Fatalf("callback error diagnostics = %+v, want failed UNKNOWN until caller classification", diag)
	}
}

// A read that never reached the provider must say so rather than implying it
// got somewhere. "unstarted" is evidence, not a placeholder.
func TestAReadThatNeverReachedTheProviderSaysUnstarted(t *testing.T) {
	_, diag, _ := BoundedRead(context.Background(), "kaneo", 20*time.Millisecond, "",
		func(ctx context.Context, p *Phases) (int, error) { <-ctx.Done(); return 0, ctx.Err() })
	if diag.Phase != "unstarted" {
		t.Fatalf("phase = %q, want unstarted for a read that recorded none", diag.Phase)
	}
}

// A provider that ignores its context must not be able to hold the report
// hostage -- waiting on it would reproduce the exact silence being fixed.
func TestAProviderIgnoringItsContextStillReportsOnTime(t *testing.T) {
	start := time.Now()
	_, diag, err := BoundedRead(context.Background(), "kaneo", 50*time.Millisecond, "",
		func(ctx context.Context, p *Phases) (int, error) {
			p.Enter("blocking call that ignores ctx")
			time.Sleep(3 * time.Second)
			return 1, nil
		})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("a read past its budget returned success")
	}
	if elapsed > time.Second {
		t.Fatalf("reporting waited %s for a context-ignoring provider; that is the rc=124 silence again", elapsed)
	}
	if diag.Outcome != ReadTimedOut {
		t.Fatalf("outcome = %q, want timed-out", diag.Outcome)
	}
}
