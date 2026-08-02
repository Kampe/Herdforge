package cron

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

type JobHandler func(ctx context.Context) error

type CronJob struct {
	ID        string
	Spec      string // Standard 5-field cron spec e.g. "0 * * * *"
	TaskRef   string
	Handler   JobHandler
	LastRun   time.Time
	RunCount  int64
}

type Scheduler struct {
	mu   sync.RWMutex
	jobs map[string]*CronJob
}

func NewScheduler() *Scheduler {
	return &Scheduler{
		jobs: make(map[string]*CronJob),
	}
}

func (s *Scheduler) AddJob(job *CronJob) error {
	if job == nil || job.ID == "" {
		return fmt.Errorf("job ID cannot be empty")
	}

	fields := strings.Fields(job.Spec)
	if len(fields) != 5 {
		return fmt.Errorf("invalid cron spec format (must be 5 fields): %s", job.Spec)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
	return nil
}

func (s *Scheduler) RemoveJob(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, id)
}

func (s *Scheduler) Tick(ctx context.Context, now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	triggered := 0
	for _, job := range s.jobs {
		// Standard simple match logic for testing & execution triggers
		if now.Sub(job.LastRun) >= time.Minute || job.LastRun.IsZero() {
			job.LastRun = now
			job.RunCount++
			triggered++
			if job.Handler != nil {
				_ = job.Handler(ctx)
			}
		}
	}
	return triggered
}
