package beat

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// IntegrationAction names one exact reviewed candidate and its integration
// owner. Action is prompt data, never a shell command executed by this queue.
type IntegrationAction struct {
	CandidateSHA string `json:"candidate_sha"`
	PullRequest  int    `json:"pull_request"`
	Task         string `json:"task"`
	Owner        string `json:"owner"`
	Target       string `json:"target"`
	Session      string `json:"session"`
	Action       string `json:"action"`
}

func (a IntegrationAction) validate() error {
	raw, err := hex.DecodeString(a.CandidateSHA)
	if err != nil || len(raw) != 20 || a.PullRequest <= 0 {
		return fmt.Errorf("integration wake requires an exact SHA and positive PR")
	}
	if strings.TrimSpace(a.Target) == "" || strings.TrimSpace(a.Session) == "" || strings.TrimSpace(a.Owner) == "" || strings.TrimSpace(a.Task) == "" || strings.TrimSpace(a.Action) == "" {
		return fmt.Errorf("integration wake requires task, owner and executable action")
	}
	return nil
}

// IntegrationWake is the latest intent for a candidate, not a per-lane slot.
// Delivery is not integration completion: only a matching acknowledgement or
// a complete observation withdrawing readiness retires this intent.
type IntegrationWake struct {
	IntegrationAction
	Generation  int64     `json:"generation"`
	CreatedAt   time.Time `json:"created_at"`
	DeliveredAt time.Time `json:"delivered_at,omitempty"`
	ConsumedAt  time.Time `json:"consumed_at,omitempty"`
	Withdrawn   bool      `json:"withdrawn,omitempty"`
	Escalated   bool      `json:"escalated,omitempty"`
}

type integrationState struct {
	Version int                        `json:"version"`
	Wakes   map[string]IntegrationWake `json:"wakes"`
}

func loadIntegrationState(path string) (integrationState, error) {
	state := integrationState{Version: 1, Wakes: map[string]IntegrationWake{}}
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	state = integrationState{}
	if err = json.Unmarshal(body, &state); err != nil {
		return state, fmt.Errorf("integration wake state: %w", err)
	}
	if state.Version != 1 || state.Wakes == nil {
		return state, fmt.Errorf("invalid integration wake state")
	}
	for sha, w := range state.Wakes {
		if err := w.validate(); err != nil {
			return state, err
		}
		if sha != w.CandidateSHA || w.Generation <= 0 || w.CreatedAt.IsZero() {
			return state, fmt.Errorf("invalid integration wake identity or generation")
		}
	}
	return state, nil
}

// saveIntegrationState is called under the shared cross-process lock. A crash
// must expose either the prior complete state or the new complete state.
func saveIntegrationState(path string, state integrationState) error {
	body, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".integration-wake-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ReconcileIntegrationWakes accepts a COMPLETE successful readiness snapshot.
// Callers must not invoke it after partial/unknown reads. An absent candidate
// withdraws its previous intent. The delivery callback must use the native
// durable transport with SHA+Generation as its idempotency identity, because
// a process can die after delivery but before DeliveredAt is persisted.
//
// The lock spans the bounded delivery call: a later snapshot or ack cannot
// race an old intent into being sent after its replacement. No callback may
// re-enter this queue. All delivered actions still require merge admission.
func ReconcileIntegrationWakes(ctx context.Context, path string, ready []IntegrationAction, now time.Time, maxAge time.Duration, deliver func(context.Context, IntegrationWake) error) ([]IntegrationWake, error) {
	if now.IsZero() || maxAge <= 0 || deliver == nil {
		return nil, fmt.Errorf("integration wake requires clock, positive escalation age and delivery")
	}
	desired := map[string]IntegrationAction{}
	for _, a := range ready {
		if err := a.validate(); err != nil {
			return nil, err
		}
		if _, ok := desired[a.CandidateSHA]; ok {
			return nil, fmt.Errorf("duplicate integration candidate %s", a.CandidateSHA)
		}
		desired[a.CandidateSHA] = a
	}
	var result []IntegrationWake
	err := envelope.WithSessionFileLock(path, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := loadIntegrationState(path)
		if err != nil {
			return err
		}
		for sha, w := range state.Wakes {
			if _, ok := desired[sha]; !ok {
				w.Withdrawn = true
				state.Wakes[sha] = w
			}
		}
		for sha, a := range desired {
			w, ok := state.Wakes[sha]
			if !ok || w.Withdrawn || w.IntegrationAction != a {
				generation := w.Generation + 1
				if generation <= 0 {
					return fmt.Errorf("integration wake generation overflow")
				}
				w = IntegrationWake{IntegrationAction: a, Generation: generation, CreatedAt: now}
			}
			if w.ConsumedAt.IsZero() && !w.Escalated && now.Sub(w.CreatedAt) >= maxAge {
				w.Generation++
				if w.Generation <= 0 {
					return fmt.Errorf("integration wake generation overflow")
				}
				w.Escalated = true
				w.DeliveredAt = time.Time{}
			}
			state.Wakes[sha] = w
		}
		// Persist every replacement/withdrawal before attempting any delivery.
		if err := saveIntegrationState(path, state); err != nil {
			return err
		}
		// Preserve the caller's canonical priority/ref order; do not replace it
		// with SHA order or duplicate the provider's priority policy here.
		keys := make([]string, 0, len(ready))
		for _, action := range ready {
			keys = append(keys, action.CandidateSHA)
		}
		var problems []error
		for _, sha := range keys {
			w := state.Wakes[sha]
			if w.Withdrawn || !w.ConsumedAt.IsZero() {
				continue
			}
			if w.DeliveredAt.IsZero() {
				if err := ctx.Err(); err != nil {
					return err
				}
				if err := deliver(ctx, w); err != nil {
					problems = append(problems, fmt.Errorf("integration wake %s generation %d: %w", sha, w.Generation, err))
				} else {
					w.DeliveredAt = now
					state.Wakes[sha] = w
					if err := saveIntegrationState(path, state); err != nil {
						return err
					}
				}
			}
			result = append(result, w)
		}
		return errors.Join(problems...)
	})
	return result, err
}

// AcknowledgeIntegrationWake refuses a delayed acknowledgement of an older
// generation. It records handling, never authorizes merge or changes a card.
func AcknowledgeIntegrationWake(path, sha string, generation int64, now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("integration acknowledgement requires a timestamp")
	}
	return envelope.WithSessionFileLock(path, func() error {
		state, err := loadIntegrationState(path)
		if err != nil {
			return err
		}
		w, ok := state.Wakes[sha]
		if !ok || w.Generation != generation || w.Withdrawn || w.DeliveredAt.IsZero() {
			return fmt.Errorf("integration acknowledgement does not match a delivered current wake")
		}
		if !w.ConsumedAt.IsZero() {
			return nil
		}
		w.ConsumedAt = now
		state.Wakes[sha] = w
		return saveIntegrationState(path, state)
	})
}
