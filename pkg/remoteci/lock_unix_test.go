//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package remoteci

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStoreLockContentionTimesOutBoundedly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settlements.jsonl")
	holder, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := syscall.Flock(int(holder.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(holder.Fd()), syscall.LOCK_UN) //nolint:errcheck

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.lockTimeout = 25 * time.Millisecond
	binding := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("8", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}
	started := time.Now()
	_, _, err = store.Register(binding)
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("Register under contention error = %v, want ErrLockTimeout", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("lock contention exceeded bounded deadline: %s", elapsed)
	}
}
