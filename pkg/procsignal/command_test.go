package procsignal

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestCommandContextKillsDescendantOnTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	cmd := CommandContext(ctx, "sh", "-c", "sleep 30")
	err := cmd.Run()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("context did not reach its deadline: %v", ctx.Err())
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected terminated command, got %v", err)
	}
}
