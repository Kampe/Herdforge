package cron

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseSchedule_ValidTable(t *testing.T) {
	tests := []struct {
		name     string
		spec     string
		testTime time.Time
		expected bool
	}{
		{
			name:     "every minute matches now",
			spec:     "* * * * *",
			testTime: time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "step minute matches on interval",
			spec:     "*/5 * * * *",
			testTime: time.Date(2026, 8, 15, 14, 25, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "step minute does not match off interval",
			spec:     "*/5 * * * *",
			testTime: time.Date(2026, 8, 15, 14, 26, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "specific hour and minute match",
			spec:     "30 14 * * *",
			testTime: time.Date(2026, 8, 15, 14, 30, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "specific hour and minute mismatch",
			spec:     "30 14 * * *",
			testTime: time.Date(2026, 8, 15, 15, 30, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "month names match (AUG)",
			spec:     "0 0 1 AUG *",
			testTime: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "month names mismatch (AUG vs SEP)",
			spec:     "0 0 1 AUG *",
			testTime: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
			expected: false,
		},
		{
			name:     "weekday range MON-FRI matches Saturday as false",
			spec:     "0 12 * * MON-FRI",
			testTime: time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC), // 2026-08-15 is Saturday
			expected: false,
		},
		{
			name:     "weekday range MON-FRI matches Monday as true",
			spec:     "0 12 * * MON-FRI",
			testTime: time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC), // 2026-08-17 is Monday
			expected: true,
		},
		{
			name:     "weekday 7 matches Sunday",
			spec:     "0 0 * * 7",
			testTime: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC), // 2026-08-16 is Sunday
			expected: true,
		},
		{
			name:     "weekday SUN matches Sunday",
			spec:     "0 0 * * SUN",
			testTime: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC), // 2026-08-16 is Sunday
			expected: true,
		},
		{
			name:     "DOM / DOW POSIX union: matches 15th on Saturday",
			spec:     "0 0 15 * 1",                                  // 15th OR Monday
			testTime: time.Date(2026, 8, 15, 0, 0, 0, 0, time.UTC), // 15th is Saturday -> matches DOM
			expected: true,
		},
		{
			name:     "DOM / DOW POSIX union: matches Monday on 17th",
			spec:     "0 0 15 * 1",                                  // 15th OR Monday
			testTime: time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC), // 17th is Monday -> matches DOW
			expected: true,
		},
		{
			name:     "DOM / DOW POSIX union: does not match 16th Sunday",
			spec:     "0 0 15 * 1",                                  // 15th OR Monday
			testTime: time.Date(2026, 8, 16, 0, 0, 0, 0, time.UTC), // 16th is Sunday -> neither matches
			expected: false,
		},
		{
			name:     "minute ranges with step",
			spec:     "10-20/5 0 * * *",
			testTime: time.Date(2026, 8, 15, 0, 15, 0, 0, time.UTC),
			expected: true,
		},
		{
			name:     "minute ranges with step off value",
			spec:     "10-20/5 0 * * *",
			testTime: time.Date(2026, 8, 15, 0, 16, 0, 0, time.UTC),
			expected: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sched, err := ParseSchedule(tc.spec)
			if err != nil {
				t.Fatalf("unexpected parse error for spec %q: %v", tc.spec, err)
			}
			got := sched.Matches(tc.testTime)
			if got != tc.expected {
				t.Errorf("spec %q at %v: expected Matches=%v, got %v", tc.spec, tc.testTime, tc.expected, got)
			}
		})
	}
}

func TestParseSchedule_InvalidTable(t *testing.T) {
	invalidSpecs := []struct {
		name string
		spec string
	}{
		{"empty string", ""},
		{"too few fields (4)", "* * * *"},
		{"too many fields (6)", "* * * * * *"},
		{"minute out of range high", "60 * * * *"},
		{"minute negative", "-1 * * * *"},
		{"hour out of range high", "* 24 * * *"},
		{"dom zero", "* * 0 * *"},
		{"dom out of range high", "* * 32 * *"},
		{"month zero", "* * * 0 *"},
		{"month out of range high", "* * * 13 *"},
		{"dow out of range high", "* * * * 8"},
		{"invalid month name", "* * * FOO *"},
		{"invalid dow name", "* * * * BAR"},
		{"step is zero", "*/0 * * * *"},
		{"reversed range", "30-10 * * * *"},
		{"non-numeric range", "a-b * * * *"},
		{"empty comma item", "1,,2 * * * *"},
		{"trailing slash", "* * * * */"},
	}

	for _, tc := range invalidSpecs {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseSchedule(tc.spec)
			if err == nil {
				t.Errorf("expected error for invalid spec %q, got nil", tc.spec)
			}
		})
	}
}

func TestScheduler_AddRemoveAndTick(t *testing.T) {
	sched := NewScheduler()

	var executed bool
	job := &CronJob{
		ID:      "gc-sweep",
		Spec:    "* * * * *",
		TaskRef: "FAC-18",
		Handler: func(ctx context.Context) error {
			executed = true
			return nil
		},
	}

	if err := sched.AddJob(job); err != nil {
		t.Fatalf("expected clean AddJob, got err: %v", err)
	}

	targetTime := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	triggered := sched.Tick(context.Background(), targetTime)
	if triggered != 1 || !executed {
		t.Errorf("expected 1 job triggered, got %d (executed: %v)", triggered, executed)
	}

	if job.GetRunCount() != 1 {
		t.Errorf("expected RunCount=1, got %d", job.GetRunCount())
	}

	if !job.GetLastRun().Equal(targetTime) {
		t.Errorf("expected LastRun=%v, got %v", targetTime, job.GetLastRun())
	}

	sched.RemoveJob("gc-sweep")
	triggered2 := sched.Tick(context.Background(), targetTime.Add(time.Minute))
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

	errEmptyID := sched.AddJob(&CronJob{ID: "", Spec: "* * * * *"})
	if errEmptyID == nil {
		t.Errorf("expected empty ID error, got nil")
	}
}

func TestScheduler_MinuteDeduplication(t *testing.T) {
	sched := NewScheduler()
	var runCount int

	job := &CronJob{
		ID:   "dedup-test",
		Spec: "* * * * *",
		Handler: func(ctx context.Context) error {
			runCount++
			return nil
		},
	}

	if err := sched.AddJob(job); err != nil {
		t.Fatalf("failed to add job: %v", err)
	}

	t0 := time.Date(2026, 8, 15, 10, 0, 15, 0, time.UTC)
	triggered1 := sched.Tick(context.Background(), t0)
	if triggered1 != 1 || runCount != 1 {
		t.Fatalf("first tick: expected triggered=1 runCount=1, got %d, %d", triggered1, runCount)
	}

	// Same minute tick at 10:00:45 -> should be deduplicated
	t0LaterInMin := time.Date(2026, 8, 15, 10, 0, 45, 0, time.UTC)
	triggered2 := sched.Tick(context.Background(), t0LaterInMin)
	if triggered2 != 0 || runCount != 1 {
		t.Errorf("same minute tick: expected triggered=0 runCount=1, got %d, %d", triggered2, runCount)
	}

	// Next minute tick at 10:01:00 -> should trigger
	t1 := time.Date(2026, 8, 15, 10, 1, 0, 0, time.UTC)
	triggered3 := sched.Tick(context.Background(), t1)
	if triggered3 != 1 || runCount != 2 {
		t.Errorf("next minute tick: expected triggered=1 runCount=2, got %d, %d", triggered3, runCount)
	}
}

func TestScheduler_Reentrancy(t *testing.T) {
	// Reentrancy: Handlers must be executed outside of Scheduler mutex.
	// This test asserts that a handler can call AddJob, RemoveJob, GetJob, and Jobs()
	// from within its own execution without deadlocking.
	sched := NewScheduler()

	var dynamicAdded bool
	parentJob := &CronJob{
		ID:   "parent",
		Spec: "* * * * *",
		Handler: func(ctx context.Context) error {
			// Query scheduler
			jobs := sched.Jobs()
			if len(jobs) == 0 {
				return fmt.Errorf("expected at least parent job")
			}

			// Add a dynamic child job
			err := sched.AddJob(&CronJob{
				ID:   "child",
				Spec: "* * * * *",
				Handler: func(ctx context.Context) error {
					return nil
				},
			})
			if err != nil {
				return err
			}
			dynamicAdded = true

			// Verify child can be queried
			_, found := sched.GetJob("child")
			if !found {
				return fmt.Errorf("child job not found immediately after AddJob")
			}

			// Remove parent job reentrantly
			sched.RemoveJob("parent")
			return nil
		},
	}

	if err := sched.AddJob(parentJob); err != nil {
		t.Fatalf("failed to add parent job: %v", err)
	}

	now := time.Date(2026, 8, 15, 10, 0, 0, 0, time.UTC)
	triggered, errs := sched.TickWithErrors(context.Background(), now)
	if triggered != 1 {
		t.Errorf("expected 1 job triggered, got %d", triggered)
	}
	if len(errs) > 0 {
		t.Fatalf("unexpected handler errors: %v", errs)
	}
	if !dynamicAdded {
		t.Errorf("expected dynamic child job to be added during handler execution")
	}

	// Verify parent was removed and child is present
	if _, ok := sched.GetJob("parent"); ok {
		t.Errorf("expected parent to be removed")
	}
	if _, ok := sched.GetJob("child"); !ok {
		t.Errorf("expected child to exist in scheduler")
	}
}

func TestScheduler_HandlerErrorExposure(t *testing.T) {
	sched := NewScheduler()

	expectedErr := errors.New("simulated database failure")
	var jobErrSeen error
	var schedErrSeen error
	var schedErrJobID string

	sched.OnError = func(jobID string, err error) {
		schedErrJobID = jobID
		schedErrSeen = err
	}

	job := &CronJob{
		ID:   "failing-job",
		Spec: "* * * * *",
		Handler: func(ctx context.Context) error {
			return expectedErr
		},
		OnError: func(err error) {
			jobErrSeen = err
		},
	}

	if err := sched.AddJob(job); err != nil {
		t.Fatalf("failed to add job: %v", err)
	}

	now := time.Date(2026, 8, 15, 11, 0, 0, 0, time.UTC)
	triggered, errs := sched.TickWithErrors(context.Background(), now)
	if triggered != 1 {
		t.Errorf("expected 1 triggered, got %d", triggered)
	}

	// Verify TickWithErrors returns the error
	if errs["failing-job"] != expectedErr {
		t.Errorf("expected error in TickWithErrors map, got %v", errs["failing-job"])
	}

	// Verify job.GetLastError()
	if job.GetLastError() != expectedErr {
		t.Errorf("expected job.GetLastError() == %v, got %v", expectedErr, job.GetLastError())
	}

	// Verify job.OnError callback
	if jobErrSeen != expectedErr {
		t.Errorf("expected job.OnError called with %v, got %v", expectedErr, jobErrSeen)
	}

	// Verify scheduler.OnError callback
	if schedErrSeen != expectedErr || schedErrJobID != "failing-job" {
		t.Errorf("expected sched.OnError called with jobID=failing-job and err=%v, got jobID=%s err=%v", expectedErr, schedErrJobID, schedErrSeen)
	}

	// Verify LastErrors() map
	lastErrors := sched.LastErrors()
	if lastErrors["failing-job"] != expectedErr {
		t.Errorf("expected LastErrors to contain failing-job error, got %v", lastErrors)
	}

	// Verify subsequent success clears the error
	job.Handler = func(ctx context.Context) error {
		return nil
	}
	nextMinute := now.Add(time.Minute)
	triggered2, errs2 := sched.TickWithErrors(context.Background(), nextMinute)
	if triggered2 != 1 {
		t.Errorf("expected 1 triggered on subsequent run, got %d", triggered2)
	}
	if len(errs2) != 0 {
		t.Errorf("expected 0 errors on successful run, got %v", errs2)
	}
	if job.GetLastError() != nil {
		t.Errorf("expected LastError to be cleared to nil, got %v", job.GetLastError())
	}
}

func TestScheduler_ConcurrencyAndOverlap(t *testing.T) {
	sched := NewScheduler()

	blockCh := make(chan struct{})
	startedCh := make(chan struct{})
	var executionCount int32

	job := &CronJob{
		ID:              "slow-job",
		Spec:            "* * * * *",
		AllowConcurrent: false,
		Handler: func(ctx context.Context) error {
			atomic.AddInt32(&executionCount, 1)
			close(startedCh)
			<-blockCh
			return nil
		},
	}

	if err := sched.AddJob(job); err != nil {
		t.Fatalf("failed to add job: %v", err)
	}

	t0 := time.Date(2026, 8, 15, 12, 0, 0, 0, time.UTC)
	go func() {
		sched.Tick(context.Background(), t0)
	}()

	// Wait until handler is actively running
	<-startedCh

	if !job.IsRunning() {
		t.Errorf("expected job.IsRunning() == true")
	}

	// Tick at next minute while previous execution is still blocked
	t1 := time.Date(2026, 8, 15, 12, 1, 0, 0, time.UTC)
	triggeredWhileRunning := sched.Tick(context.Background(), t1)
	if triggeredWhileRunning != 0 {
		t.Errorf("expected non-concurrent job to skip overlapping run, got triggered=%d", triggeredWhileRunning)
	}
	if count := atomic.LoadInt32(&executionCount); count != 1 {
		t.Errorf("expected executionCount=1, got %d", count)
	}

	// Unblock handler
	close(blockCh)
	time.Sleep(20 * time.Millisecond)

	if job.IsRunning() {
		t.Errorf("expected job.IsRunning() == false after completion")
	}

	// Subsequent tick now triggers cleanly
	t2 := time.Date(2026, 8, 15, 12, 2, 0, 0, time.UTC)
	job.Handler = func(ctx context.Context) error {
		atomic.AddInt32(&executionCount, 1)
		return nil
	}
	triggeredAfter := sched.Tick(context.Background(), t2)
	if triggeredAfter != 1 {
		t.Errorf("expected job to trigger after finishing, got %d", triggeredAfter)
	}
	if count := atomic.LoadInt32(&executionCount); count != 2 {
		t.Errorf("expected executionCount=2, got %d", count)
	}
}

func TestScheduler_ConcurrentRace(t *testing.T) {
	sched := NewScheduler()

	const numWorkers = 8
	const numIterations = 50

	var wg sync.WaitGroup
	wg.Add(numWorkers)

	for w := 0; w < numWorkers; w++ {
		workerID := w
		go func() {
			defer wg.Done()
			for i := 0; i < numIterations; i++ {
				jobID := fmt.Sprintf("job-%d-%d", workerID, i%5)
				job := &CronJob{
					ID:   jobID,
					Spec: "* * * * *",
					Handler: func(ctx context.Context) error {
						return nil
					},
				}

				_ = sched.AddJob(job)
				_, _ = sched.GetJob(jobID)
				_ = sched.Jobs()
				_ = sched.LastErrors()

				tickTime := time.Date(2026, 8, 15, 12, i%60, 0, 0, time.UTC)
				_ = sched.Tick(context.Background(), tickTime)

				if i%3 == 0 {
					sched.RemoveJob(jobID)
				}
			}
		}()
	}

	wg.Wait()
}
