package integration

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// Intent survives an interruption between an external effect and its receipt.
// ID is an idempotency key for the backend, not proof that the effect happened.
type Intent struct {
	ID        string `json:"id"`
	Candidate string `json:"candidate"`
	Step      Step   `json:"step"`
	StartedAt string `json:"started_at"`
}

// EffectState describes an authoritative readback, never a transport outcome.
type EffectState string

const (
	EffectUnknown EffectState = "unknown"
	EffectAbsent  EffectState = "absent"
	EffectApplied EffectState = "applied"
)

// Observation binds real external evidence to the exact attempted operation.
// Absent means the backend has proved it is safe to execute with the SAME ID;
// a timeout, failed read, or partially applied effect must remain Unknown.
type Observation struct {
	Intent   Intent
	State    EffectState
	Evidence string
}

// Backend implements the native operations and their independent readbacks.
// Check and Observe are read-only. Check must enforce the current step's live
// admission/canonicality gates; a durable intent never substitutes for them.
// Execute must honor Intent.ID across retries. It may return an error after an
// external effect succeeds; Advance retains that intent for later observation.
// A backend must not call Advance or Save while the transaction lock is held.
type Backend interface {
	Check(context.Context, Transaction, Intent) error
	Observe(context.Context, Transaction, Intent) (Observation, error)
	Execute(context.Context, Transaction, Intent) error
}

// Advance performs at most ONE lifecycle step. The caller must name the step
// it intends to advance: replaying a stale invocation cannot merge or clean up
// simply because an earlier invocation already progressed the transaction.
//
// The cross-process file lock covers every transition and external callback.
// A crash releases the kernel lock while leaving the fsynced intent behind.
// Resumption observes that same intent before considering another execution;
// an ambiguous observation refuses without destroying the recovery handle.
func Advance(ctx context.Context, repoRoot, candidate string, expected Step, backend Backend) (*Record, error) {
	canonical, err := New(candidate)
	if err != nil {
		return nil, err
	}
	if len(canonical.Candidate) != 40 {
		return nil, fmt.Errorf("integration execution requires a full 40-character candidate sha")
	}
	if backend == nil {
		return nil, fmt.Errorf("integration: native step backend is required")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var result *Record
	err = envelope.WithSessionFileLock(Path(repoRoot, canonical.Candidate), func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		tx, err := Load(repoRoot, canonical.Candidate)
		if err != nil {
			return err
		}
		// Old manual --step/--evidence records are useful history, but they do
		// not prove that this driver observed the native effects.
		if tx.DriverVersion == 0 && len(tx.Done) != 0 {
			return fmt.Errorf("integration: manual history cannot authorize native execution")
		}
		for _, r := range tx.Done {
			if r.Step == expected {
				r := r
				result = &r
				return nil
			}
		}
		next, more := tx.Next()
		if !more || expected != next {
			return fmt.Errorf("integration: requested %q, next step is %q; refusing out-of-order execution", expected, next)
		}
		intent := tx.Pending
		if intent == nil {
			id := make([]byte, 16)
			if _, err := rand.Read(id); err != nil {
				return err
			}
			intent = &Intent{ID: hex.EncodeToString(id), Candidate: tx.Candidate, Step: next, StartedAt: time.Now().UTC().Format(time.RFC3339)}
		}
		if err := backend.Check(ctx, snapshot(tx), *intent); err != nil {
			return fmt.Errorf("integration %s admission: %w", next, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if tx.Pending == nil {
			tx.DriverVersion = 1
			tx.Pending = intent
			if err := saveLocked(repoRoot, tx); err != nil {
				return fmt.Errorf("integration: intent was not durably saved; no effect attempted: %w", err)
			}
		}
		observed, err := observe(ctx, backend, tx, *intent)
		if err != nil {
			return err
		}
		if observed.State == EffectAbsent {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := backend.Execute(ctx, snapshot(tx), *intent); err != nil {
				return fmt.Errorf("integration %s execution: %w; intent retained for readback", next, err)
			}
			// A zero exit is insufficient: independently read back the effect.
			observed, err = observe(ctx, backend, tx, *intent)
			if err != nil {
				return err
			}
		}
		if observed.State != EffectApplied || strings.TrimSpace(observed.Evidence) == "" {
			return fmt.Errorf("integration %s: effect is not proven applied; intent retained", next)
		}
		tx.Pending = nil
		if err := tx.complete(next, observed.Evidence, intent.ID); err != nil {
			return err
		}
		if err := saveLocked(repoRoot, tx); err != nil {
			return fmt.Errorf("integration %s: effect observed but completion not persisted: %w", next, err)
		}
		r := tx.Done[len(tx.Done)-1]
		result = &r
		return nil
	})
	return result, err
}

func observe(ctx context.Context, backend Backend, tx *Transaction, intent Intent) (Observation, error) {
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	o, err := backend.Observe(ctx, snapshot(tx), intent)
	if err != nil {
		return Observation{}, fmt.Errorf("integration %s readback: %w; intent retained", intent.Step, err)
	}
	if o.Intent != intent {
		return Observation{}, fmt.Errorf("integration %s: readback names a different execution identity", intent.Step)
	}
	switch o.State {
	case EffectAbsent, EffectApplied:
		return o, nil
	default:
		return Observation{}, fmt.Errorf("integration %s: unknown effect outcome; refusing execution, intent retained", intent.Step)
	}
}

func validOperationID(id string) bool {
	return len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}

func snapshot(tx *Transaction) Transaction {
	copy := *tx
	copy.Done = append([]Record(nil), tx.Done...)
	if tx.Pending != nil {
		p := *tx.Pending
		copy.Pending = &p
	}
	return copy
}
