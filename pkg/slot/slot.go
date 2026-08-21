// Package slot coordinates machine-wide resource-heavy phases.
package slot

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	EnvDirectory   = "HERD_HEAVY_PHASE_SLOT_DIR"
	EnvCount       = "HERD_HEAVY_PHASE_SLOTS"
	EnvMaxAge      = "HERD_HEAVY_PHASE_SLOT_MAX_AGE"
	EnvTimeout     = "HERD_HEAVY_PHASE_SLOT_TIMEOUT"
	EnvHeld        = "HERD_HEAVY_PHASE_SLOT_HELD"
	DefaultCount   = 1
	DefaultMaxAge  = 30 * time.Minute
	DefaultTimeout = 30 * time.Minute
)

type Semaphore struct {
	dir    string
	count  int
	maxAge time.Duration
}

type Lease struct {
	sem   *Semaphore
	slot  string
	token string
	held  bool
}

func (l *Lease) Slot() int {
	if l == nil {
		return -1
	}
	n, _ := strconv.Atoi(filepath.Base(l.slot))
	return n
}
func (l *Lease) Token() string {
	if l == nil {
		return ""
	}
	return l.token
}

func (s *Semaphore) Release(slotNumber int, token string) error {
	if slotNumber < 0 || slotNumber >= s.count || strings.TrimSpace(token) == "" {
		return errors.New("heavy-phase slots: invalid release")
	}
	return (&Lease{sem: s, slot: filepath.Join(s.dir, strconv.Itoa(slotNumber)), token: token}).Release()
}

type Holder struct {
	Slot    int    `json:"slot"`
	Purpose string `json:"purpose,omitempty"`
	PID     int    `json:"pid"`
	Token   string `json:"token"`
}

func New(dir string, count int) (*Semaphore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("heavy-phase slots: directory is empty")
	}
	if count < 1 {
		return nil, fmt.Errorf("heavy-phase slots: count must be positive, got %d", count)
	}
	return &Semaphore{dir: filepath.Clean(dir), count: count, maxAge: DefaultMaxAge}, nil
}

func Default() (*Semaphore, error) {
	dir := strings.TrimSpace(os.Getenv(EnvDirectory))
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "herd-heavy-phase-slots")
	}
	count := DefaultCount
	if raw := strings.TrimSpace(os.Getenv(EnvCount)); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			return nil, fmt.Errorf("heavy-phase slots: invalid %s=%q", EnvCount, raw)
		}
		count = n
	}
	s, err := New(dir, count)
	if err != nil {
		return nil, err
	}
	if raw := strings.TrimSpace(os.Getenv(EnvMaxAge)); raw != "" {
		age, err := time.ParseDuration(raw)
		if err != nil || age <= 0 {
			return nil, fmt.Errorf("heavy-phase slots: invalid %s=%q", EnvMaxAge, raw)
		}
		s.maxAge = age
	}
	return s, nil
}

func (s *Semaphore) Directory() string           { return s.dir }
func (s *Semaphore) Count() int                  { return s.count }
func (s *Semaphore) SetMaxAge(age time.Duration) { s.maxAge = age }

func (s *Semaphore) Acquire(ctx context.Context, purpose string, wait time.Duration) (*Lease, error) {
	if s == nil || s.count < 1 || s.dir == "" {
		return nil, errors.New("heavy-phase slots: invalid semaphore")
	}
	if os.Getenv(EnvHeld) == "1" {
		return &Lease{sem: s, held: true}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if wait < 0 {
		return nil, errors.New("heavy-phase slots: wait must not be negative")
	}
	deadline := time.Now().Add(wait)
	for {
		if err := os.MkdirAll(s.dir, 0o755); err != nil {
			return nil, fmt.Errorf("heavy-phase slots: create directory: %w", err)
		}
		for i := 0; i < s.count; i++ {
			dir := filepath.Join(s.dir, strconv.Itoa(i))
			s.breakIfStale(dir)
			if err := os.Mkdir(dir, 0o755); err == nil {
				token := randomToken()
				if err := writeHolder(filepath.Join(dir, "holder"), purpose, token, processStartToken(os.Getpid())); err != nil {
					_ = os.Remove(dir)
					return nil, fmt.Errorf("heavy-phase slots: write holder: %w", err)
				}
				return &Lease{sem: s, slot: dir, token: token}, nil
			} else if !errors.Is(err, os.ErrExist) {
				return nil, fmt.Errorf("heavy-phase slots: acquire slot %d: %w", i, err)
			}
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("heavy-phase slots: all %d slots busy, waited %s", s.count, wait)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

func (l *Lease) Release() error {
	if l == nil || l.sem == nil || l.held || l.slot == "" {
		return nil
	}
	holder, err := os.ReadFile(filepath.Join(l.slot, "holder"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if !strings.Contains(string(holder), "token="+l.token+"\n") {
		return fmt.Errorf("heavy-phase slots: lease ownership changed")
	}
	if err := os.RemoveAll(l.slot); err != nil {
		return fmt.Errorf("heavy-phase slots: release: %w", err)
	}
	l.slot = ""
	return nil
}

func (s *Semaphore) With(ctx context.Context, purpose string, wait time.Duration, fn func() error) error {
	lease, err := s.Acquire(ctx, purpose, wait)
	if err != nil {
		return err
	}
	defer func() { _ = lease.Release() }()
	return fn()
}

func (s *Semaphore) Status() []Holder {
	if s == nil {
		return nil
	}
	var holders []Holder
	for i := 0; i < s.count; i++ {
		path := filepath.Join(s.dir, strconv.Itoa(i), "holder")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var h Holder
		h.Slot = i
		for _, line := range strings.Split(string(data), "\n") {
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			switch key {
			case "pid":
				h.PID, _ = strconv.Atoi(value)
			case "purpose":
				h.Purpose = value
			case "token":
				h.Token = value
			}
		}
		holders = append(holders, h)
	}
	return holders
}

func (s *Semaphore) breakIfStale(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	data, _ := os.ReadFile(filepath.Join(dir, "holder"))
	pid := 0
	startedAt := ""
	for _, line := range strings.Split(string(data), "\n") {
		if v, ok := strings.CutPrefix(line, "pid="); ok {
			pid, _ = strconv.Atoi(v)
		} else if v, ok := strings.CutPrefix(line, "start="); ok {
			startedAt = v
		}
	}
	if pid > 0 {
		err = syscall.Kill(pid, 0)
		if err == nil || errors.Is(err, syscall.EPERM) {
			// A live PID alone does not prove it's still the same process:
			// the recorded holder's PID can have been reused by an unrelated
			// process after the real holder exited. Cross-check the process
			// start time recorded at acquire time; a mismatch (or a startup
			// that can no longer be read back) means the original holder is
			// gone, so the slot is reclaimed immediately instead of waiting
			// out maxAge against a process that was never the true holder.
			if startedAt != "" && processStartToken(pid) == startedAt {
				if time.Since(info.ModTime()) <= s.maxAge {
					return false
				}
			} else {
				_ = os.RemoveAll(dir)
				return true
			}
		} else if errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EINVAL) {
			_ = os.RemoveAll(dir)
			return true
		}
	}
	if time.Since(info.ModTime()) > s.maxAge {
		_ = os.RemoveAll(dir)
		return true
	}
	return false
}

func writeHolder(path, purpose, token, startedAt string) error {
	data := fmt.Sprintf("pid=%d\npurpose=%s\ntoken=%s\nstart=%s\n", os.Getpid(), strings.ReplaceAll(purpose, "\n", " "), token, startedAt)
	return os.WriteFile(path, []byte(data), 0o644)
}

// processStartToken fingerprints a live PID by its process start time, the
// same identity primitive pkg/toolchild uses to detect PID reuse. Returns ""
// when the lookup fails, e.g. the PID is already gone.
func processStartToken(pid int) string {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func randomToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("pid-%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
