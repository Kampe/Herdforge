package cron

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// JobHandler defines the callback function executed when a CronJob triggers.
type JobHandler func(ctx context.Context) error

// Schedule represents a parsed 5-field cron schedule.
// Fields: minute (0-59), hour (0-23), day of month (1-31), month (1-12), day of week (0-6, 0=Sunday).
type Schedule struct {
	minutes uint64 // bitset for minutes 0-59
	hours   uint32 // bitset for hours 0-23
	dom     uint32 // bitset for day of month 1-31 (bits 1-31)
	months  uint16 // bitset for month 1-12 (bits 1-12)
	dow     uint8  // bitset for day of week 0-6 (0=Sunday ... 6=Saturday)
	domStar bool   // true if Day of Month was specified as wildcard '*'
	dowStar bool   // true if Day of Week was specified as wildcard '*'
}

// Matches reports whether the schedule matches the specified time instant.
func (s Schedule) Matches(t time.Time) bool {
	minute := t.Minute()
	hour := t.Hour()
	dom := t.Day()
	month := int(t.Month())
	dow := int(t.Weekday())

	if (s.minutes & (uint64(1) << minute)) == 0 {
		return false
	}
	if (s.hours & (uint32(1) << hour)) == 0 {
		return false
	}
	if (s.months & (uint16(1) << month)) == 0 {
		return false
	}

	domMatch := (s.dom & (uint32(1) << dom)) != 0
	dowMatch := (s.dow & (uint8(1) << dow)) != 0

	// POSIX/Vixie cron DOM & DOW matching semantics:
	// If both DOM and DOW are wildcard '*', any day matches.
	// If one is '*' and the other is restricted, the restricted field must match.
	// If neither is '*', matching either (OR) is sufficient.
	if s.domStar && s.dowStar {
		return true
	}
	if s.domStar {
		return dowMatch
	}
	if s.dowStar {
		return domMatch
	}
	return domMatch || dowMatch
}

var monthNames = map[string]int{
	"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4,
	"MAY": 5, "JUN": 6, "JUL": 7, "AUG": 8,
	"SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12,
}

var dowNames = map[string]int{
	"SUN": 0, "MON": 1, "TUE": 2, "WED": 3,
	"THU": 4, "FRI": 5, "SAT": 6,
}

// ParseSchedule parses and validates a standard 5-field cron spec string:
// "minute hour day-of-month month day-of-week"
func ParseSchedule(spec string) (Schedule, error) {
	fields := strings.Fields(spec)
	if len(fields) != 5 {
		return Schedule{}, fmt.Errorf("invalid cron spec %q: expected 5 fields, got %d", spec, len(fields))
	}

	minBits, _, err := parseField(fields[0], 0, 59, nil, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid minute field %q: %w", fields[0], err)
	}

	hourBits, _, err := parseField(fields[1], 0, 23, nil, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid hour field %q: %w", fields[1], err)
	}

	domBits, domStar, err := parseField(fields[2], 1, 31, nil, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid day-of-month field %q: %w", fields[2], err)
	}

	monthBits, _, err := parseField(fields[3], 1, 12, monthNames, false)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid month field %q: %w", fields[3], err)
	}

	dowBits, dowStar, err := parseField(fields[4], 0, 7, dowNames, true)
	if err != nil {
		return Schedule{}, fmt.Errorf("invalid day-of-week field %q: %w", fields[4], err)
	}

	return Schedule{
		minutes: minBits,
		hours:   uint32(hourBits),
		dom:     uint32(domBits),
		months:  uint16(monthBits),
		dow:     uint8(dowBits),
		domStar: domStar,
		dowStar: dowStar,
	}, nil
}

func parseField(fieldStr string, min, max int, names map[string]int, isDOW bool) (uint64, bool, error) {
	if fieldStr == "" {
		return 0, false, fmt.Errorf("field cannot be empty")
	}

	if fieldStr == "*" {
		var bits uint64
		effMax := max
		if isDOW && max == 7 {
			effMax = 6
		}
		for i := min; i <= effMax; i++ {
			bits |= uint64(1) << uint(i)
		}
		return bits, true, nil
	}

	parts := strings.Split(fieldStr, ",")
	var bitset uint64

	for _, part := range parts {
		if part == "" {
			return 0, false, fmt.Errorf("empty entry in list")
		}

		// Check for step: e.g. "*/5" or "1-10/2" or "0/15"
		step := 1
		rangePart := part
		if strings.Contains(part, "/") {
			stepSplit := strings.Split(part, "/")
			if len(stepSplit) != 2 || stepSplit[0] == "" || stepSplit[1] == "" {
				return 0, false, fmt.Errorf("invalid step expression %q", part)
			}
			sVal, err := strconv.Atoi(stepSplit[1])
			if err != nil || sVal <= 0 {
				return 0, false, fmt.Errorf("invalid step value %q in %q (must be positive integer)", stepSplit[1], part)
			}
			step = sVal
			rangePart = stepSplit[0]
		}

		var start, end int
		if rangePart == "*" {
			start = min
			end = max
			if isDOW && end == 7 {
				end = 6
			}
		} else if strings.Contains(rangePart, "-") {
			rangeSplit := strings.Split(rangePart, "-")
			if len(rangeSplit) != 2 || rangeSplit[0] == "" || rangeSplit[1] == "" {
				return 0, false, fmt.Errorf("invalid range expression %q", rangePart)
			}
			s, err := parseValue(rangeSplit[0], min, max, names)
			if err != nil {
				return 0, false, err
			}
			e, err := parseValue(rangeSplit[1], min, max, names)
			if err != nil {
				return 0, false, err
			}
			if s > e {
				return 0, false, fmt.Errorf("invalid reversed range %d-%d", s, e)
			}
			start = s
			end = e
		} else {
			val, err := parseValue(rangePart, min, max, names)
			if err != nil {
				return 0, false, err
			}
			if step > 1 {
				start = val
				end = max
				if isDOW && end == 7 {
					end = 6
				}
			} else {
				start = val
				end = val
			}
		}

		for v := start; v <= end; v += step {
			target := v
			if isDOW && target == 7 {
				target = 0
			}
			bitset |= uint64(1) << uint(target)
		}
	}

	return bitset, false, nil
}

func parseValue(tok string, min, max int, names map[string]int) (int, error) {
	tok = strings.TrimSpace(tok)
	if val, err := strconv.Atoi(tok); err == nil {
		if val < min || val > max {
			return 0, fmt.Errorf("value %d out of range [%d, %d]", val, min, max)
		}
		return val, nil
	}

	if names != nil {
		if val, ok := names[strings.ToUpper(tok)]; ok {
			if val < min || val > max {
				return 0, fmt.Errorf("value %s (%d) out of range [%d, %d]", tok, val, min, max)
			}
			return val, nil
		}
	}

	return 0, fmt.Errorf("invalid value %q", tok)
}

// CronJob represents a scheduled task in the cron engine.
//
// Concurrency and Reentrancy Semantics:
//   - Reentrancy: Handlers are never invoked while holding the Scheduler mutex.
//     Handlers are free to query, add, or remove jobs from the Scheduler without deadlocking.
//   - Concurrency: If AllowConcurrent is false (the default), overlapping executions of
//     the same job instance are suppressed if a prior execution is still running when a new
//     tick triggers. If AllowConcurrent is true, overlapping executions are permitted.
//   - Error Handling: Errors returned by handlers are recorded in LastError, forwarded to
//     the OnError hook, and reported by TickWithErrors.
type CronJob struct {
	ID              string
	Spec            string // Standard 5-field cron spec e.g. "0 * * * *"
	TaskRef         string
	Handler         JobHandler
	LastRun         time.Time
	RunCount        int64
	LastError       error
	AllowConcurrent bool            // Allow overlapping executions if previous invocation is still active
	OnError         func(err error) // Optional per-job error callback

	schedule Schedule
	mu       sync.Mutex
	running  bool
}

// GetLastError returns the most recent error produced by the job's handler, thread-safely.
func (j *CronJob) GetLastError() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.LastError
}

// GetLastRun returns the timestamp of the job's most recent execution, thread-safely.
func (j *CronJob) GetLastRun() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.LastRun
}

// GetRunCount returns the total number of times the job was triggered, thread-safely.
func (j *CronJob) GetRunCount() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.RunCount
}

// IsRunning returns true if the job is currently executing.
func (j *CronJob) IsRunning() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.running
}

// Scheduler coordinates scheduled cron jobs.
// It is fully thread-safe for concurrent additions, removals, reads, and ticks.
type Scheduler struct {
	mu      sync.RWMutex
	jobs    map[string]*CronJob
	OnError func(jobID string, err error) // Optional scheduler-level error callback
}

// NewScheduler creates an empty, ready-to-use cron Scheduler.
func NewScheduler() *Scheduler {
	return &Scheduler{
		jobs: make(map[string]*CronJob),
	}
}

// AddJob registers a new CronJob. The job's 5-field Spec is parsed and validated.
// Returns an error if job is nil, job ID is empty, or the cron Spec is invalid.
func (s *Scheduler) AddJob(job *CronJob) error {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return fmt.Errorf("job ID cannot be empty")
	}

	sched, err := ParseSchedule(job.Spec)
	if err != nil {
		return fmt.Errorf("invalid cron spec %q for job %s: %w", job.Spec, job.ID, err)
	}

	job.schedule = sched

	s.mu.Lock()
	defer s.mu.Unlock()
	s.jobs[job.ID] = job
	return nil
}

// RemoveJob unregisters a job by ID from the scheduler.
func (s *Scheduler) RemoveJob(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.jobs, id)
}

// GetJob retrieves a job by ID.
func (s *Scheduler) GetJob(id string) (*CronJob, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	job, ok := s.jobs[id]
	return job, ok
}

// Jobs returns a snapshot slice of all registered jobs.
func (s *Scheduler) Jobs() []*CronJob {
	s.mu.RLock()
	defer s.mu.RUnlock()
	list := make([]*CronJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		list = append(list, j)
	}
	return list
}

// LastErrors returns a map of jobID -> LastError for all jobs with a non-nil LastError.
func (s *Scheduler) LastErrors() map[string]error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	errs := make(map[string]error)
	for id, j := range s.jobs {
		if err := j.GetLastError(); err != nil {
			errs[id] = err
		}
	}
	return errs
}

// Tick evaluates all registered jobs against the provided time, triggers matching jobs,
// and returns the count of triggered jobs. Handlers are executed strictly outside of the
// scheduler mutex to guarantee reentrancy and prevent deadlocks.
func (s *Scheduler) Tick(ctx context.Context, now time.Time) int {
	triggered, _ := s.TickWithErrors(ctx, now)
	return triggered
}

// TickWithErrors evaluates all registered jobs against now, triggers matching jobs,
// and returns the count of triggered jobs alongside any handler errors encountered during execution.
// Handlers are executed outside the scheduler lock.
func (s *Scheduler) TickWithErrors(ctx context.Context, now time.Time) (int, map[string]error) {
	s.mu.RLock()
	candidates := make([]*CronJob, 0, len(s.jobs))
	for _, j := range s.jobs {
		candidates = append(candidates, j)
	}
	s.mu.RUnlock()

	currentMinute := now.Truncate(time.Minute)
	type execution struct {
		job     *CronJob
		handler JobHandler
	}
	var toRun []execution

	for _, job := range candidates {
		if !job.schedule.Matches(now) {
			continue
		}

		job.mu.Lock()
		// Prevent multiple executions within the same minute
		if !job.LastRun.IsZero() && job.LastRun.Truncate(time.Minute).Equal(currentMinute) {
			job.mu.Unlock()
			continue
		}

		// Prevent overlapping executions if AllowConcurrent is false
		if !job.AllowConcurrent && job.running {
			job.mu.Unlock()
			continue
		}

		job.LastRun = now
		job.RunCount++
		job.running = true
		handler := job.Handler
		job.mu.Unlock()

		toRun = append(toRun, execution{
			job:     job,
			handler: handler,
		})
	}

	if len(toRun) == 0 {
		return 0, nil
	}

	errors := make(map[string]error)
	for _, exec := range toRun {
		var runErr error
		if exec.handler != nil {
			// Handlers are invoked outside of both s.mu and job.mu
			runErr = exec.handler(ctx)
		}

		exec.job.mu.Lock()
		exec.job.running = false
		exec.job.LastError = runErr
		jobOnError := exec.job.OnError
		exec.job.mu.Unlock()

		if runErr != nil {
			errors[exec.job.ID] = runErr
			if jobOnError != nil {
				jobOnError(runErr)
			}
			if s.OnError != nil {
				s.OnError(exec.job.ID, runErr)
			}
		}
	}

	return len(toRun), errors
}
