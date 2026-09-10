package worktreebootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ConsumerIdentity is the authenticated dispatch identity carried into the
// managed bootstrap boundary. Empty identity is rejected; legacy bootstrap
// calls therefore cannot silently enroll a cache.
type ConsumerIdentity struct {
	Repository      string
	TaskRef         string
	LeaseGeneration int64
	Worktree        string
}

type ProcessIdentity struct {
	PID        int    `json:"pid"`
	ParentPID  int    `json:"parent_pid"`
	StartToken string `json:"start_token"`
}

type CacheUseLease struct {
	Version         int              `json:"version"`
	CacheDir        string           `json:"cache_dir"`
	ToolchainDigest string           `json:"toolchain_digest"`
	Consumer        ConsumerIdentity `json:"consumer"`
	Process         ProcessIdentity  `json:"process"`
	ExpiresAt       time.Time        `json:"expires_at"`
}

const cacheUseLeasePath = ".herd/bootstrap/cache-use.json"

var cacheUseMu sync.Mutex

func acquireCacheUseFileLock(worktree string) (func(), error) {
	path := filepath.Join(worktree, filepath.FromSlash(".herd/bootstrap/cache-use.lock"))
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open managed cache-use lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock managed cache-use registry: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func validateConsumer(id ConsumerIdentity) error {
	if id.Repository == "" || id.TaskRef == "" || id.LeaseGeneration <= 0 || id.Worktree == "" {
		return errors.New("managed bootstrap consumer identity is incomplete")
	}
	return nil
}

func acquireCacheUseLease(worktree string, receipt Receipt, consumer ConsumerIdentity, process ProcessIdentity, ttl time.Duration) (func(), error) {
	if err := validateConsumer(consumer); err != nil {
		return nil, err
	}
	if process.PID <= 0 || process.ParentPID <= 0 || process.StartToken == "" || ttl <= 0 {
		return nil, errors.New("managed bootstrap process identity is incomplete")
	}
	if filepath.IsAbs(consumer.Worktree) || filepath.Clean(consumer.Worktree) == "." || strings.Contains(filepath.ToSlash(consumer.Worktree), "../") {
		return nil, errors.New("managed bootstrap worktree identity is not repository-relative")
	}
	cacheUseMu.Lock()
	path := filepath.Join(worktree, filepath.FromSlash(cacheUseLeasePath))
	if data, err := os.ReadFile(path); err == nil && len(data) != 0 {
		cacheUseMu.Unlock()
		return nil, errors.New("managed bootstrap cache-use lease already exists")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		cacheUseMu.Unlock()
		return nil, fmt.Errorf("read managed cache-use lease: %w", err)
	}
	lease := CacheUseLease{Version: 1, CacheDir: receipt.CacheDir, ToolchainDigest: receipt.ToolchainDigest, Consumer: consumer, Process: process, ExpiresAt: time.Now().Add(ttl)}
	data, err := json.Marshal(lease)
	if err != nil {
		cacheUseMu.Unlock()
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cache-use-*")
	if err != nil {
		cacheUseMu.Unlock()
		return nil, err
	}
	tmpName := tmp.Name()
	if err = tmp.Chmod(0o600); err == nil {
		_, err = tmp.Write(data)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Link(tmpName, path)
		_ = os.Remove(tmpName)
	}
	if err != nil {
		_ = os.Remove(tmpName)
		cacheUseMu.Unlock()
		return nil, fmt.Errorf("write managed cache-use lease: %w", err)
	}
	return func() {
		defer cacheUseMu.Unlock()
		_ = os.Remove(path)
	}, nil
}
