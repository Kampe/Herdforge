// Package coordinator provides the durable identity registration for the
// forge loop coordinator.
//
// FAC-222: before this package, no dispatched packet carried a reply address.
// Agents had nowhere to report completion or BLOCKED, so the coordinator
// discovered every finished branch by polling git state — late and lossy. A
// branch that reset itself to origin/main was found by an anomalous ahead=0;
// an empty-diff PR merged and was caught only by a later audit.
//
// Register writes a durable JSON file (.herd/coordinator.json) at forge-loop
// start so the coordinator has a stable name that dispatch can embed in every
// TASK-PACKET.md and that the feedback census can resolve without relying on
// a herdr agent-list match (the coordinator is a Go process, not a tmux
// pane — herdr agent list will never list it).
package coordinator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// CoordinatorName is the stable name every agent addresses reports to. It
// matches mail.CoordinatorInbox so a callback posted to the durable mailbox
// and a herd-send addressed by name both reach the same coordinator identity.
const CoordinatorName = "coordinator"

// RegistrationFile is the repo-relative path of the durable coordinator
// identity record. It is written atomically at forge-loop start and read by
// dispatch (to embed the reply target in TASK-PACKET.md) and by the feedback
// census (to resolve the coordinator target without a herdr agent-list match).
const RegistrationFile = ".herd/coordinator.json"

// Registration is the durable coordinator identity record.
type Registration struct {
	Name      string    `json:"name"`
	Workspace string    `json:"workspace,omitempty"`
	StartedAt time.Time `json:"started_at"`
	PID       int       `json:"pid,omitempty"`
}

// registrationPath returns the absolute path to the registration file under
// root. root may be relative ("."). The path is always cleaned.
func registrationPath(root string) string {
	if root == "" {
		root = "."
	}
	return filepath.Join(root, RegistrationFile)
}

// Register writes the durable coordinator identity. It is called once at
// forge-loop start. A non-empty name is required; when callers pass an empty
// name, CoordinatorName is used. The workspace is the resolved herdr
// workspace id (may be empty when the fleet config does not set one — the
// feedback census resolves it independently).
//
// The file is written atomically so a crash mid-write cannot leave a
// half-parsed identity behind. The parent directory is created if missing.
// Overwriting a prior registration is intentional: a new loop instance
// supersedes a stale one.
func Register(root, workspace string) (*Registration, error) {
	name := CoordinatorName
	reg := &Registration{
		Name:      name,
		Workspace: workspace,
		StartedAt: time.Now().UTC(),
		PID:       os.Getpid(),
	}
	path := registrationPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("coordinator: create registration dir: %w", err)
	}
	body, err := json.Marshal(reg)
	if err != nil {
		return nil, fmt.Errorf("coordinator: marshal registration: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return nil, fmt.Errorf("coordinator: write registration: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return nil, fmt.Errorf("coordinator: commit registration: %w", err)
	}
	return reg, nil
}

// Resolve reads the durable coordinator identity. Absence is NOT an error:
// it means no coordinator has registered yet, and the caller falls back to
// the default coordinator name. A corrupt file IS an error — silently
// discarding it would let dispatch address reports to a name the coordinator
// never claimed, which is how a lane that destroyed its own commits went
// unnoticed.
func Resolve(root string) (*Registration, error) {
	path := registrationPath(root)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &Registration{Name: CoordinatorName}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("coordinator: read registration %s: %w", path, err)
	}
	if len(raw) == 0 {
		return &Registration{Name: CoordinatorName}, nil
	}
	var reg Registration
	if err := json.Unmarshal(raw, &reg); err != nil {
		return nil, fmt.Errorf("coordinator: corrupt registration %s: %w", path, err)
	}
	if reg.Name == "" {
		reg.Name = CoordinatorName
	}
	return &reg, nil
}
