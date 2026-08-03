package daemon

// Durable disk-pressure projection (FAC-153): BLOCKED -> recovering ->
// ready transitions are persisted as redacted JSONL under the repository's
// lifecycle state root with a readback API, so acceptance evidence exists
// beyond process memory. Evidence is role/path-redacted at the guard layer
// (DiskStat.Path is json:"-"; bounded vol-<fsid> labels only). Provider/
// Kaneo board writes stay with the fenced FAC-147 projection owner — this
// file supplies the durable evidence and readback that path consumes.

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// DiskProjectionRecord is one persisted state transition.
type DiskProjectionRecord struct {
	At     time.Time `json:"at"`
	State  string    `json:"state"`  // ok | blocked | recovering
	Status string    `json:"status"` // ready | recovering | BLOCKED(<reason>)
	Reason string    `json:"reason,omitempty"`
	Detail string    `json:"detail,omitempty"`
	// Volumes carry bounded labels, fs identity, and values — never paths.
	Volumes                  []preflight.DiskStat `json:"volumes,omitempty"`
	OutstandingReservedBytes uint64               `json:"outstanding_reserved_bytes,omitempty"`
}

// diskProjectionSink persists guard transitions durably (append-only JSONL).
type diskProjectionSink struct{ path string }

func (s diskProjectionSink) RecordDiskState(state preflight.DiskGuardState, ev *preflight.DiskPressureError) {
	rec := DiskProjectionRecord{At: time.Now(), State: string(state)}
	switch state {
	case preflight.DiskBlocked:
		rec.Status = "BLOCKED(disk_pressure)"
	case preflight.DiskRecovering:
		rec.Status = "recovering"
	default:
		rec.Status = "ready"
	}
	if ev != nil {
		rec.Reason = ev.Reason
		rec.Detail = ev.Detail
		rec.Volumes = ev.Stats
		rec.OutstandingReservedBytes = ev.OutstandingReservedBytes
		if state == preflight.DiskBlocked {
			rec.Status = "BLOCKED(" + ev.Reason + ")"
		}
	}
	data, err := json.Marshal(rec)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return
	}
	f, err := os.OpenFile(s.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// DiskProjectionPath is the durable projection location under a repo root.
func DiskProjectionPath(repoRoot string) string {
	return filepath.Join(repoRoot, ".herd", "state", "disk-pressure.jsonl")
}

// InstallDiskProjection wires the production evidence sink for this repo:
// every guard state transition is durably appended for later readback.
func InstallDiskProjection(repoRoot string) {
	preflight.DefaultDiskGuard.SetEvidenceSink(diskProjectionSink{path: DiskProjectionPath(repoRoot)})
}

// ReadDiskProjection reads back the persisted transition history.
func ReadDiskProjection(repoRoot string) ([]DiskProjectionRecord, error) {
	f, err := os.Open(DiskProjectionPath(repoRoot))
	if err != nil {
		return nil, fmt.Errorf("disk projection readback: %w", err)
	}
	defer f.Close()
	var out []DiskProjectionRecord
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		var r DiskProjectionRecord
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return nil, fmt.Errorf("disk projection readback: %w", err)
		}
		out = append(out, r)
	}
	return out, sc.Err()
}
