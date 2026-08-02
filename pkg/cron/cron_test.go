package cron

import (
	"context"
	"testing"
	"time"
)

func TestScheduler_AddRemoveAndTick(t *testing.T) {
	sched := NewScheduler()

	var executed bool
	job := &CronJob{
		ID:      "gc-sweep",
		Spec:    "0 * * * *",
		TaskRef: "FAC-18",
		Handler: func(ctx context.Context) error {
			executed = true
			return nil
		},
	}

	if err := sched.AddJob(job); err != nil {
		t.Fatalf("expected clean AddJob, got err: %v", err)
	}

	triggered := sched.Tick(context.Background(), time.Now())
	if triggered != 1 || !executed {
		t.Errorf("expected 1 job triggered, got %d (executed: %v)", triggered, executed)
	}

	sched.RemoveJob("gc-sweep")
	triggered2 := sched.Tick(context.Background(), time.Now())
	if triggered2 != 0 {
		t.Errorf("expected 0 jobs after removal, got %d", triggered2)
	}
}

func TestScheduler_InvalidSpec(t *testing.T) {
	sched := NewScheduler()
	err := sched.AddJob(&CronJob{ID: "invalid", Spec: "invalid spec"})
	if err == nil {
		t.Errorf("expected invalid spec error, got nil")
	}
}
