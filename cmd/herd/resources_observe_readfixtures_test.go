//go:build darwin || linux || freebsd || netbsd || openbsd

package main

// FAC-829: the read fixtures the coordinator asked for. A status path that is a
// FIFO, oversized, or malformed must be REFUSED by the CLI, and refused without
// blocking. These run remotely like every other dynamic test here.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// TestObserverStatusCommandRefusesAFifo proves the reader neither blocks on nor
// trusts a non-regular file. A plain open of a FIFO with no writer never
// returns, so a hang here is the failure, not just a wrong exit code.
func TestObserverStatusCommandRefusesAFifo(t *testing.T) {
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	path := resources.ObserverStatusPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatalf("create observer directory: %v", err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	done := make(chan int, 1)
	go func() { done <- runResourcesObserverStatus(false) }()
	select {
	case code := <-done:
		if code != observerExitRefused {
			t.Fatalf("a FIFO status exited %d, expected %d", code, observerExitRefused)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("reading a FIFO status blocked; the open is not non-blocking")
	}
}

// TestObserverStatusCommandRefusesOversizeAndMalformed pins the other two read
// refusals on the real CLI path.
func TestObserverStatusCommandRefusesOversizeAndMalformed(t *testing.T) {
	cases := map[string][]byte{
		"oversize":  make([]byte, resources.MaxObserverStatusBytes+1),
		"malformed": []byte("{this is not json"),
		"truncated": []byte(`{"schema_version":1,"latest":`),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Setenv("HERD_STATE_DIR", t.TempDir())
			path := resources.ObserverStatusPath()
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatalf("create observer directory: %v", err)
			}
			if name == "oversize" {
				for i := range body {
					body[i] = ' '
				}
			}
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatalf("write %s status: %v", name, err)
			}
			if code := runResourcesObserverStatus(false); code != observerExitRefused {
				t.Fatalf("a %s status exited %d, expected %d", name, code, observerExitRefused)
			}
		})
	}
}
