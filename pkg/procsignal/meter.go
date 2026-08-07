package procsignal

import (
	"os"
	"sync/atomic"
	"syscall"
)

// Meter records refused ambient targets and host kills observed by this
// process. SafetyReceipt derives AmbientAttempts from the meter — callers
// cannot mint a zero ambient count by argument.
type Meter struct {
	ambientRefused atomic.Int64
	hostKills      atomic.Int64
	unsafeRefused  atomic.Int64
}

// defaultMeter is the process-wide meter used by validators and hostBackend.
var defaultMeter Meter

func (m *Meter) noteAmbientRefused() {
	if m == nil {
		return
	}
	m.ambientRefused.Add(1)
}

func (m *Meter) noteHostKill() {
	if m == nil {
		return
	}
	m.hostKills.Add(1)
}

// AmbientRefused returns how many host-wide/sentinel targets were refused.
func (m *Meter) AmbientRefused() int64 {
	if m == nil {
		return 0
	}
	return m.ambientRefused.Load()
}

// HostKills returns how many times hostBackend reached syscall.Kill.
func (m *Meter) HostKills() int64 {
	if m == nil {
		return 0
	}
	return m.hostKills.Load()
}

// ResetForTest clears counters. Test-only.
func (m *Meter) ResetForTest() {
	if m == nil {
		return
	}
	m.ambientRefused.Store(0)
	m.hostKills.Store(0)
	m.unsafeRefused.Store(0)
}

// DefaultMeter returns the process-wide meter.
func DefaultMeter() *Meter { return &defaultMeter }

// SafetyReceipt is task-bound evidence for a destructive or verification run.
// AmbientAttempts is observed from the meter, not supplied by the caller.
type SafetyReceipt struct {
	TaskRef         string `json:"task_ref"`
	UID             int    `json:"uid"`
	PID             int    `json:"pid"`
	Pgid            int    `json:"pgid"`
	Hermetic        bool   `json:"hermetic"`
	IsolationMode   string `json:"isolation_mode,omitempty"`
	IsolationID     string `json:"isolation_id,omitempty"`
	FixtureUID      string `json:"fixture_uid,omitempty"`
	Command         string `json:"command,omitempty"`
	AmbientAttempts int64  `json:"ambient_signal_attempts"`
	HostKills       int64  `json:"host_kills"`
}

// SnapshotReceipt builds a receipt from live process identity and meter state.
func (m *Meter) SnapshotReceipt(taskRef, command string) SafetyReceipt {
	if m == nil {
		m = &defaultMeter
	}
	return SafetyReceipt{
		TaskRef:         taskRef,
		UID:             os.Getuid(),
		PID:             os.Getpid(),
		Pgid:            syscall.Getpgrp(),
		Hermetic:        os.Getenv(HermeticEnv) == "1",
		IsolationMode:   os.Getenv(IsolationEnv),
		IsolationID:     os.Getenv(IsolationIDEnv),
		FixtureUID:      os.Getenv(FixtureUIDEnv),
		Command:         command,
		AmbientAttempts: m.AmbientRefused(),
		HostKills:       m.HostKills(),
	}
}

// NewSafetyReceipt is the package entrypoint for a meter-derived receipt.
func NewSafetyReceipt(taskRef, command string) SafetyReceipt {
	return defaultMeter.SnapshotReceipt(taskRef, command)
}
