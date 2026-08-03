package provider

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Kaneo-only production path tests (FAC-150 scope update): CLI + HTTP
// hermetic fakes. Never contacts a live board. Non-Kaneo activation is FAC-155.

func TestKaneoCLI_UpdateStatus_TimeoutReconcilesLanded(t *testing.T) {
	var statusCalls, getCalls atomic.Int32
	prev := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = prev })
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "task status"):
			statusCalls.Add(1)
			<-ctx.Done()
			return &CLIResult{}, &TimeoutError{Provider: "kaneo", Op: "UpdateStatus", Kind: OpMutate, Cause: ctx.Err()}
		case strings.Contains(joined, "task get"):
			getCalls.Add(1)
			body := `{"id":"task-1","ref":"FAC-1","title":"t","status":"in-progress","priority":"low","projectId":"p1","labels":[]}`
			return &CLIResult{Stdout: []byte(body)}, nil
		default:
			return nil, errors.New("unexpected kaneo args: " + joined)
		}
	}

	kp := NewKaneoProvider("", "p1", true)
	kp.Deadlines = Deadlines{Mutate: time.Second, Get: time.Second, Readback: time.Second}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	if err := kp.UpdateStatus(context.Background(), "task-1", StatusInProgress); err != nil {
		t.Fatalf("landed CLI write should reconcile clean: %v", err)
	}
	if statusCalls.Load() != 1 {
		t.Fatalf("status calls=%d want 1 (double-apply?)", statusCalls.Load())
	}
	if getCalls.Load() < 1 {
		t.Fatal("expected readback get after timeout")
	}
}

func TestKaneoCLI_UpdateStatus_TimeoutNotLandedNoDoubleApply(t *testing.T) {
	var statusCalls atomic.Int32
	prev := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = prev })
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		joined := strings.Join(args, " ")
		switch {
		case strings.Contains(joined, "task status"):
			statusCalls.Add(1)
			<-ctx.Done()
			return &CLIResult{}, &TimeoutError{Cause: ctx.Err(), Kind: OpMutate}
		case strings.Contains(joined, "task get"):
			body := `{"id":"task-1","ref":"FAC-1","title":"t","status":"to-do","priority":"low","projectId":"p1","labels":[]}`
			return &CLIResult{Stdout: []byte(body)}, nil
		default:
			return nil, errors.New("unexpected: " + joined)
		}
	}

	kp := NewKaneoProvider("", "p1", true)
	kp.Deadlines = Deadlines{Mutate: time.Second, Get: time.Second, Readback: time.Second}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}

	err := kp.UpdateStatus(context.Background(), "task-1", StatusInProgress)
	if !IsAmbiguous(err) {
		t.Fatalf("want AmbiguousMutationError, got %T %v", err, err)
	}
	if ClassifyOpError(err) != OpAmbiguous {
		t.Fatalf("class=%q want provider_ambiguous", ClassifyOpError(err))
	}
	if statusCalls.Load() != 1 {
		t.Fatalf("must not re-run status after lost write: calls=%d", statusCalls.Load())
	}
}

func TestKaneoCLI_Comment_TimeoutIsAmbiguous(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process-group hang")
	}
	// Real process-group kill path (not inject): hung comment CLI.
	dir := t.TempDir()
	stub := "#!/bin/sh\nsleep 300\n"
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	kp.Deadlines = Deadlines{Comment: 80 * time.Millisecond}
	err := kp.AddComment(context.Background(), "task-1", "note")
	if !IsAmbiguous(err) {
		t.Fatalf("comment timeout must be ambiguous (non-idempotent): %v", err)
	}
	var ae *AmbiguousMutationError
	if !errors.As(err, &ae) || ae.Op != "AddComment" {
		t.Fatalf("want AddComment AmbiguousMutationError, got %v", err)
	}
	if ClassifyOpError(err) != OpAmbiguous {
		t.Fatalf("class=%q", ClassifyOpError(err))
	}
}

func TestKaneoCLI_Comment_IncludesProject(t *testing.T) {
	var gotArgs []string
	prev := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = prev })
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		gotArgs = append([]string{}, args...)
		return &CLIResult{}, nil
	}
	kp := NewKaneoProvider("", "p1", true)
	if err := kp.AddComment(context.Background(), "task-1", "hi"); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(gotArgs, " ")
	if !strings.Contains(joined, "--project p1") {
		t.Fatalf("comment CLI must pin project: %v", gotArgs)
	}
}

func TestKaneoCLI_GetTask_DeadlineUsesRunCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX")
	}
	dir := t.TempDir()
	// Never-exit stub: proves process-group kill still bounds UseCLI GetTask.
	if err := os.WriteFile(filepath.Join(dir, "kaneo"), []byte("#!/bin/sh\nsleep 300\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	kp := NewKaneoProvider("", "p1", true)
	kp.Deadlines = Deadlines{Get: 80 * time.Millisecond}
	kp.Retry = RetryPolicy{MaxAttempts: 1, Clock: &fakeClock{}}
	start := time.Now()
	_, err := kp.GetTask(context.Background(), "task-1")
	elapsed := time.Since(start)
	if !IsTimeout(err) {
		t.Fatalf("want timeout, got %v", err)
	}
	if ClassifyOpError(err) != OpTimeout {
		t.Fatalf("class=%q want provider_timeout", ClassifyOpError(err))
	}
	if elapsed > 2*time.Second {
		t.Fatalf("CLI get hang took %v", elapsed)
	}
}

func TestKaneo_UpdateStatus_WritesCanonicalStatus(t *testing.T) {
	// HTTP path: "In Progress" alias must PATCH as in-progress for readback match.
	var patched string
	kp := NewKaneoProvider("http://kaneo.test", "p", false)
	kp.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodPatch {
			body, _ := io.ReadAll(req.Body)
			patched = string(body)
			return jsonResponse(http.StatusOK, `{}`), nil
		}
		return jsonResponse(http.StatusOK, `{"id":"task-1","ref":"FAC-1","title":"t","status":"in-progress","priority":"low","projectId":"p","labels":[]}`), nil
	})}
	if err := kp.UpdateStatus(context.Background(), "task-1", "In Progress"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(patched, `"status":"in-progress"`) {
		t.Fatalf("PATCH body=%q want canonical in-progress", patched)
	}
}

func TestKaneoCLI_UpdateStatus_PassesCanonicalToCLI(t *testing.T) {
	var statusArg string
	prev := kaneoRunCLI
	t.Cleanup(func() { kaneoRunCLI = prev })
	kaneoRunCLI = func(ctx context.Context, name string, args ...string) (*CLIResult, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "task status") {
			// kaneo task status <id> <status> --project <p>
			for i, a := range args {
				if a == "status" && i+2 < len(args) {
					statusArg = args[i+2]
				}
			}
			return &CLIResult{}, nil
		}
		if strings.Contains(joined, "task get") {
			body := `{"id":"task-1","ref":"FAC-1","title":"t","status":"in-progress","priority":"low","projectId":"p1","labels":[]}`
			return &CLIResult{Stdout: []byte(body)}, nil
		}
		return &CLIResult{}, nil
	}
	kp := NewKaneoProvider("", "p1", true)
	if err := kp.UpdateStatus(context.Background(), "task-1", "In Progress"); err != nil {
		t.Fatal(err)
	}
	if statusArg != StatusInProgress {
		t.Fatalf("CLI status arg=%q want %q", statusArg, StatusInProgress)
	}
}
