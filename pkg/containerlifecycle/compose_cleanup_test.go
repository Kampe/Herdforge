package containerlifecycle

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

type composeFake struct {
	stacks []ComposeStack
	down   []string
	err    error
}

func (f *composeFake) List(_ context.Context) ([]ComposeStack, error) { return f.stacks, f.err }
func (f *composeFake) Down(_ context.Context, project, _ string) error {
	f.down = append(f.down, project)
	return nil
}

func TestReapVerifyStacksAgeOwnershipAndLiveness(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fake := &composeFake{stacks: []ComposeStack{
		{Project: "old-dead", WorkingDir: ".", VerifyHarness: true, VerifyRunState: VerifyRunFinished, CreatedAt: now.Add(-2 * time.Hour)},
		{Project: "old-live", WorkingDir: ".", VerifyHarness: true, VerifyRunState: VerifyRunLive, CreatedAt: now.Add(-2 * time.Hour)},
		{Project: "old-unknown", WorkingDir: ".", VerifyHarness: true, CreatedAt: now.Add(-2 * time.Hour)},
		{Project: "young-dead", WorkingDir: ".", VerifyHarness: true, VerifyRunState: VerifyRunFinished, CreatedAt: now.Add(-5 * time.Minute)},
		{Project: "other", WorkingDir: ".", CreatedAt: now.Add(-2 * time.Hour)},
	}}
	report, err := ReapVerifyStacks(context.Background(), fake, ReapVerifyStacksOptions{MaxAge: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("ReapVerifyStacks: %v", err)
	}
	if !reflect.DeepEqual(fake.down, []string{"old-dead"}) {
		t.Fatalf("down=%v, want only old-dead", fake.down)
	}
	if !reflect.DeepEqual(report.Reaped, []string{"old-dead"}) {
		t.Fatalf("reaped=%v", report.Reaped)
	}
	if len(report.Skipped) != 2 || len(report.Blocked) != 1 {
		t.Fatalf("report=%+v", report)
	}
}

func TestReapVerifyStacksDryRunDoesNotDown(t *testing.T) {
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	fake := &composeFake{stacks: []ComposeStack{{Project: "old", WorkingDir: ".", VerifyHarness: true, VerifyRunState: VerifyRunFinished, CreatedAt: now.Add(-2 * time.Hour)}}}
	report, err := ReapVerifyStacks(context.Background(), fake, ReapVerifyStacksOptions{DryRun: true, MaxAge: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if len(fake.down) != 0 || !reflect.DeepEqual(report.WouldReap, []string{"old"}) {
		t.Fatalf("report=%+v down=%v", report, fake.down)
	}
}

func TestReapVerifyStacksFailsClosedOnListOrDownError(t *testing.T) {
	fake := &composeFake{err: errors.New("docker unavailable")}
	if _, err := ReapVerifyStacks(context.Background(), fake, ReapVerifyStacksOptions{}); err == nil {
		t.Fatal("list error must propagate")
	}
	fake = &composeFake{stacks: []ComposeStack{{Project: "old", WorkingDir: ".", VerifyHarness: true, VerifyRunState: VerifyRunFinished, CreatedAt: time.Now().Add(-time.Hour)}}}
	// A down failure is returned and the stack is not reported as reaped.
	fakeDown := &failingComposeFake{composeFake: fake}
	if _, err := ReapVerifyStacks(context.Background(), fakeDown, ReapVerifyStacksOptions{MaxAge: time.Minute}); err == nil {
		t.Fatal("down error must propagate")
	}
}

type failingComposeFake struct{ *composeFake }

func (f *failingComposeFake) Down(context.Context, string, string) error {
	return errors.New("down failed")
}
