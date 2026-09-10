package worktree

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// provenDeadPID returns a pid that provably no longer exists, so a lock
// record naming it proves the holder is gone on this host.
func provenDeadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawn transient holder: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if errors.Is(syscall.Kill(cmd.Process.Pid, 0), syscall.ESRCH) {
			return cmd.Process.Pid
		}
		if time.Now().After(deadline) {
			t.Fatalf("transient holder pid %d never disappeared", cmd.Process.Pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeIdlePoolTickLock(t *testing.T, repoRoot string, identity idlePoolTickLockIdentity) {
	t.Helper()
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	writeIdlePoolTickLockRaw(t, repoRoot, string(data))
}

func TestIdlePoolTickLockRecoversProvenDeadHolder(t *testing.T) {
	root := t.TempDir()
	writeIdlePoolTickLock(t, root, idlePoolTickLockIdentity{
		Version:   idlePoolTickLockIdentityVersion,
		Host:      mustHostname(t),
		PID:       provenDeadPID(t),
		StartedAt: time.Now().UTC(),
	})

	ran := false
	err := withIdlePoolTickLock(root, func() error { ran = true; return nil })
	if err != nil {
		t.Fatalf("lock left by a proven-dead holder must be recovered, not wedged: %v", err)
	}
	if !ran {
		t.Fatal("recovered lock never ran the tick")
	}
	if _, err := os.Stat(idlePoolLockPath(root)); !os.IsNotExist(err) {
		t.Fatalf("lock must be released after the tick: stat err=%v", err)
	}
}

func TestIdlePoolTickLockSparesLiveHolder(t *testing.T) {
	root := t.TempDir()
	writeIdlePoolTickLock(t, root, idlePoolTickLockIdentity{
		Version:   idlePoolTickLockIdentityVersion,
		Host:      mustHostname(t),
		PID:       os.Getpid(), // the test process itself: provably live
		StartedAt: time.Now().UTC(),
	})

	ran := false
	err := withIdlePoolTickLock(root, func() error { ran = true; return nil })
	if !errors.Is(err, ErrIdlePoolTickBusy) {
		t.Fatalf("live holder must report the benign busy sentinel, got err=%v", err)
	}
	if ran {
		t.Fatal("tick must not run while another live holder owns the lock")
	}
	if _, statErr := os.Stat(idlePoolLockPath(root)); statErr != nil {
		t.Fatalf("possibly live lock must never be unlinked: %v", statErr)
	}
}

func TestIdlePoolTickLockRefusesUnprovableLock(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		host  string
		title string
	}{
		{name: "corrupt record", body: "{not json", title: "corrupt"},
		{name: "version mismatch", body: `{"version":999,"host":"h","pid":1}`, title: "version"},
		{name: "foreign host", body: `{"version":1,"host":"other-host","pid":1}`, title: "foreign"},
		{name: "host unreadable", body: `{"version":1,"pid":1}`, title: "hostless"},
		{name: "pidless", body: `{"version":1,"host":"` + mustHostname(t) + `","pid":0}`, title: "pidless"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			writeIdlePoolTickLockRaw(t, root, tc.body)
			ran := false
			err := withIdlePoolTickLock(root, func() error { ran = true; return nil })
			if !errors.Is(err, ErrIdlePoolTickBusy) {
				t.Fatalf("%s lock must stay busy (never age-unlinked), got err=%v", tc.title, err)
			}
			if ran {
				t.Fatal("tick must not run under an unprovable lock")
			}
			if _, statErr := os.Stat(idlePoolLockPath(root)); statErr != nil {
				t.Fatalf("unprovable lock must be preserved for forensics: %v", statErr)
			}
		})
	}
}

func TestIdlePoolTickLockRecordsOwnerGenerationIdentity(t *testing.T) {
	root := t.TempDir()
	err := withIdlePoolTickLock(root, func() error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	// After a clean tick the lock is gone; the identity contract is that a
	// lock file present mid-tick is parseable owner/generation evidence.
	release := make(chan struct{})
	holderErr := make(chan error, 1)
	go func() {
		holderErr <- withIdlePoolTickLock(root, func() error { <-release; return nil })
	}()
	deadline := time.Now().Add(5 * time.Second)
	for {
		data, readErr := os.ReadFile(idlePoolLockPath(root))
		if readErr == nil {
			var identity idlePoolTickLockIdentity
			if json.Unmarshal(data, &identity) != nil {
				t.Fatalf("live lock must carry parseable owner/generation identity: %s", string(data))
			}
			if identity.Version != idlePoolTickLockIdentityVersion || identity.PID != os.Getpid() || identity.StartedAt.IsZero() {
				t.Fatalf("live lock identity=%+v", identity)
			}
			close(release)
			if goErr := <-holderErr; goErr != nil {
				t.Fatalf("holder tick: %v", goErr)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("holder never acquired the lock: %v", readErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func writeIdlePoolTickLockRaw(t *testing.T, repoRoot, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(repoRoot, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(idlePoolLockPath(repoRoot), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mustHostname(t *testing.T) string {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("hostname unavailable: %v", err)
	}
	if strings.TrimSpace(host) == "" {
		t.Skip("hostname empty")
	}
	return host
}
