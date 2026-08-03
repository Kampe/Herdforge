package preflight

// Cross-process disk reservation ledger (FAC-153). Herdforge runs many
// independent CLI/agent/test processes against the same host volumes, so
// reservation accounting must be host-scoped, not process memory: one JSON
// file per reservation, keyed by filesystem identity, with owner heartbeat,
// crash expiry, and exact release. The ledger only ever deletes its OWN
// expired entry files — never any fleet artifact.

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	// EnvDiskLedgerDir overrides the host-scoped ledger directory
	// (default: <tmp>/herd-disk-ledger).
	EnvDiskLedgerDir = "HERD_DISK_LEDGER_DIR"
	// EnvDiskLeaseTTLSec is the heartbeat-staleness expiry for process
	// leases (default 300s). A crashed owner stops heartbeating and its
	// reservation expires.
	EnvDiskLeaseTTLSec = "HERD_DISK_LEASE_TTL_SEC"
	// EnvDiskTaskLeaseTTLSec is the wall-clock expiry for task-session
	// leases, which intentionally survive the creating process across the
	// worktree+build+verify interval (default 7200s).
	EnvDiskTaskLeaseTTLSec = "HERD_DISK_TASK_LEASE_TTL_SEC"

	defaultLeaseTTLSec     = 300
	defaultTaskLeaseTTLSec = 7200
)

var ledgerSeq atomic.Uint64

type ledgerEntry struct {
	ID          string    `json:"id"`
	FSID        string    `json:"fsid"`
	Bytes       uint64    `json:"bytes"`
	Operation   string    `json:"operation"`
	Session     string    `json:"session,omitempty"` // task ref for session leases
	PID         int       `json:"pid"`
	CreatedAt   time.Time `json:"created_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	TTLSec      int64     `json:"ttl_sec"`
}

type diskLedger struct{}

func ledgerDir() string {
	if d := os.Getenv(EnvDiskLedgerDir); d != "" {
		return d
	}
	return filepath.Join(os.TempDir(), "herd-disk-ledger")
}

func envInt(key string, def int64) int64 {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// fsidDir maps a filesystem identity to a fixed-width ledger directory
// (FNV-1a) so arbitrary-length identities can never exceed filename
// limits; a hash collision merely shares accounting, which is the
// conservative direction.
func fsidDir(fsid string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(fsid))
	return filepath.Join(ledgerDir(), fmt.Sprintf("%016x", h.Sum64()))
}

func (diskLedger) entryPath(fsid, id string) string {
	return filepath.Join(fsidDir(fsid), id+".json")
}

// reserve writes one reservation entry atomically (tmp + rename).
func (l diskLedger) reserve(e ledgerEntry) error {
	dir := fsidDir(e.FSID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("disk ledger: %w", err)
	}
	data, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("disk ledger: %w", err)
	}
	tmp := filepath.Join(dir, e.ID+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("disk ledger: %w", err)
	}
	if err := os.Rename(tmp, l.entryPath(e.FSID, e.ID)); err != nil {
		return fmt.Errorf("disk ledger: %w", err)
	}
	return nil
}

// release removes exactly one reservation entry.
func (l diskLedger) release(fsid, id string) {
	_ = os.Remove(l.entryPath(fsid, id))
}

// heartbeat refreshes a process lease's liveness stamp.
func (l diskLedger) heartbeat(fsid, id string) {
	p := l.entryPath(fsid, id)
	data, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var e ledgerEntry
	if json.Unmarshal(data, &e) != nil {
		return
	}
	e.HeartbeatAt = time.Now()
	if out, err := json.Marshal(e); err == nil {
		_ = os.WriteFile(p, out, 0o600)
	}
}

func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// expired reports whether an entry no longer holds capacity. Process
// leases expire when the owner is dead AND the heartbeat is stale (either
// alone is insufficient: pid reuse / a live-but-stalled owner). Session
// leases expire on wall-clock TTL only — they intentionally outlive the
// creating process.
func (e ledgerEntry) expired(now time.Time) bool {
	ttl := time.Duration(e.TTLSec) * time.Second
	if e.Session != "" {
		return now.Sub(e.CreatedAt) > ttl
	}
	stale := now.Sub(e.HeartbeatAt) > ttl
	return stale || !pidAlive(e.PID)
}

// outstanding sums live reservations for one filesystem identity, pruning
// expired entries (crash expiry) as it goes. Read failures fail closed.
func (l diskLedger) outstanding(fsid string) (uint64, error) {
	dir := fsidDir(fsid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("disk ledger unreadable: %w", err)
	}
	now := time.Now()
	var sum uint64
	for _, de := range entries {
		if de.IsDir() || filepath.Ext(de.Name()) != ".json" {
			continue
		}
		p := filepath.Join(dir, de.Name())
		data, err := os.ReadFile(p)
		if err != nil {
			return 0, fmt.Errorf("disk ledger unreadable: %w", err)
		}
		var e ledgerEntry
		if json.Unmarshal(data, &e) != nil || e.expired(now) {
			// Crash expiry: remove only our own dead ledger artifact.
			_ = os.Remove(p)
			continue
		}
		sum = satAdd(sum, e.Bytes)
	}
	return sum, nil
}

// releaseSession removes every entry belonging to a task session across
// all filesystem identities.
func (l diskLedger) releaseSession(session string) {
	root := ledgerDir()
	dirs, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, d := range dirs {
		if !d.IsDir() {
			continue
		}
		sub := filepath.Join(root, d.Name())
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			p := filepath.Join(sub, f.Name())
			data, err := os.ReadFile(p)
			if err != nil {
				continue
			}
			var e ledgerEntry
			if json.Unmarshal(data, &e) == nil && e.Session == session {
				_ = os.Remove(p)
			}
		}
	}
}

func newLedgerID() string {
	return fmt.Sprintf("%d-%d-%d", os.Getpid(), time.Now().UnixNano(), ledgerSeq.Add(1))
}
