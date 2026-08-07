// Package broadcast compiles a task-bound fleet prompt target set and
// subtracts protected, quarantined, reviewer, and historical lanes before
// any prompt is delivered (FAC-187).
//
// Every prompt is gated by an exact identity check (task, generation,
// session, cwd/worktree, role, allowed action). Selected and excluded
// targets are recorded in a durable receipt so a compensated failure and a
// successful delivery leave the same audit trail.
package broadcast

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ExclusionKind is why a candidate is removed from the delivery set.
type ExclusionKind string

const (
	ExcludeProtected   ExclusionKind = "protected"
	ExcludeQuarantined ExclusionKind = "quarantined"
	ExcludeReviewer    ExclusionKind = "reviewer"
	ExcludeHistorical  ExclusionKind = "historical"
)

// AllExclusionKinds is the closed set applied by Select. Order is stable for
// receipts when a target carries multiple markers.
var AllExclusionKinds = []ExclusionKind{
	ExcludeProtected,
	ExcludeQuarantined,
	ExcludeReviewer,
	ExcludeHistorical,
}

// Target is one addressable fleet lane for a single prompt delivery.
type Target struct {
	Name           string          `json:"name"`
	TaskRef        string          `json:"task_ref,omitempty"`
	PaneID         string          `json:"pane_id,omitempty"`
	TabID          string          `json:"tab_id,omitempty"`
	Workspace      string          `json:"workspace,omitempty"`
	Cwd            string          `json:"cwd,omitempty"`
	Role           string          `json:"role,omitempty"`
	Session        string          `json:"session,omitempty"`
	Generation     int64           `json:"generation,omitempty"`
	AllowedActions []string        `json:"allowed_actions,omitempty"`
	Markers        []ExclusionKind `json:"markers,omitempty"`
}

// Excluded records a candidate that must never receive the prompt.
type Excluded struct {
	Target Target        `json:"target"`
	Reason ExclusionKind `json:"reason"`
}

// Selection is the compiled target set after exclusion subtraction.
type Selection struct {
	Selected []Target   `json:"selected"`
	Excluded []Excluded `json:"excluded"`
}

// primaryExclusion returns the first matching exclusion kind in stable order,
// or empty when the target is eligible.
func primaryExclusion(markers []ExclusionKind) ExclusionKind {
	set := make(map[ExclusionKind]struct{}, len(markers))
	for _, m := range markers {
		set[m] = struct{}{}
	}
	for _, kind := range AllExclusionKinds {
		if _, ok := set[kind]; ok {
			return kind
		}
	}
	return ""
}

// Select subtracts every protected/quarantined/reviewer/historical candidate
// and returns a deterministic selection (selected and excluded sorted by name,
// then task ref).
func Select(candidates []Target) Selection {
	var sel Selection
	for _, c := range candidates {
		if reason := primaryExclusion(c.Markers); reason != "" {
			sel.Excluded = append(sel.Excluded, Excluded{Target: c, Reason: reason})
			continue
		}
		sel.Selected = append(sel.Selected, c)
	}
	sort.SliceStable(sel.Selected, func(i, j int) bool {
		return targetLess(sel.Selected[i], sel.Selected[j])
	})
	sort.SliceStable(sel.Excluded, func(i, j int) bool {
		return targetLess(sel.Excluded[i].Target, sel.Excluded[j].Target)
	})
	return sel
}

func targetLess(a, b Target) bool {
	if a.Name != b.Name {
		return a.Name < b.Name
	}
	return a.TaskRef < b.TaskRef
}

// PromptIdentity is the exact pre-prompt fence. Every field that is set on
// the bound identity must match the live identity; AllowedActions must
// contain Action.
type PromptIdentity struct {
	TaskRef        string
	Generation     int64
	Session        string
	Cwd            string
	Role           string
	AllowedActions []string
	Action         string
}

var (
	// ErrIdentityMismatch is returned when live target drift is detected.
	ErrIdentityMismatch = errors.New("broadcast: prompt identity mismatch")
	// ErrActionDenied is returned when the requested action is not allowed.
	ErrActionDenied = errors.New("broadcast: action not allowed")
	// ErrIncompleteIdentity is returned when required identity fields are empty.
	ErrIncompleteIdentity = errors.New("broadcast: incomplete prompt identity")
)

// CheckIdentity verifies bound and live identities immediately before a
// prompt. Task, generation, cwd, and role are always required. Session is
// directional: empty bound means "not yet observed"; a bound non-empty
// session must match live.
func CheckIdentity(bound, live PromptIdentity) error {
	if strings.TrimSpace(bound.TaskRef) == "" || bound.Generation <= 0 ||
		strings.TrimSpace(bound.Cwd) == "" || strings.TrimSpace(bound.Role) == "" ||
		strings.TrimSpace(bound.Action) == "" {
		return fmt.Errorf("%w: task, generation, cwd, role, and action are required", ErrIncompleteIdentity)
	}
	if bound.TaskRef != live.TaskRef {
		return fmt.Errorf("%w: task_ref bound=%q live=%q", ErrIdentityMismatch, bound.TaskRef, live.TaskRef)
	}
	if bound.Generation != live.Generation {
		return fmt.Errorf("%w: generation bound=%d live=%d", ErrIdentityMismatch, bound.Generation, live.Generation)
	}
	if bound.Cwd != live.Cwd {
		return fmt.Errorf("%w: cwd bound=%q live=%q", ErrIdentityMismatch, bound.Cwd, live.Cwd)
	}
	if bound.Role != live.Role {
		return fmt.Errorf("%w: role bound=%q live=%q", ErrIdentityMismatch, bound.Role, live.Role)
	}
	if bound.Session != "" && bound.Session != live.Session {
		return fmt.Errorf("%w: session bound=%q live=%q", ErrIdentityMismatch, bound.Session, live.Session)
	}
	if !actionAllowed(bound.AllowedActions, bound.Action) {
		return fmt.Errorf("%w: %q not in %v", ErrActionDenied, bound.Action, bound.AllowedActions)
	}
	if !actionAllowed(live.AllowedActions, bound.Action) {
		return fmt.Errorf("%w: live lane denies %q", ErrActionDenied, bound.Action)
	}
	return nil
}

func actionAllowed(list []string, action string) bool {
	for _, a := range list {
		if a == action {
			return true
		}
	}
	return false
}

// IdentityFromTarget builds a PromptIdentity for action from a Target.
func IdentityFromTarget(t Target, action string) PromptIdentity {
	return PromptIdentity{
		TaskRef:        t.TaskRef,
		Generation:     t.Generation,
		Session:        t.Session,
		Cwd:            t.Cwd,
		Role:           t.Role,
		AllowedActions: append([]string(nil), t.AllowedActions...),
		Action:         action,
	}
}

// PromptFunc delivers text to one selected target. Tests inject a recorder;
// production wires herdr.DeliverAndProve or herdr.AgentPrompt.
type PromptFunc func(ctx context.Context, target Target, text string) error

// LiveIdentityFunc re-reads the live identity for a target immediately before
// prompt. Required on the production path.
type LiveIdentityFunc func(ctx context.Context, target Target) (PromptIdentity, error)

// Deliverer runs a broadcast: select → identity-check → prompt once per
// eligible target. Excluded lanes never see the prompt.
type Deliverer struct {
	Prompt PromptFunc
	Live   LiveIdentityFunc
	Now    func() time.Time
}

// TargetReceipt records one selected delivery attempt.
type TargetReceipt struct {
	Target  Target `json:"target"`
	Prompted bool  `json:"prompted"`
	Error   string `json:"error,omitempty"`
}

// Receipt is the durable record of one broadcast.
type Receipt struct {
	ID        string          `json:"id"`
	Action    string          `json:"action"`
	At        time.Time       `json:"at"`
	Selected  []TargetReceipt `json:"selected"`
	Excluded  []Excluded      `json:"excluded"`
	Compensated []string      `json:"compensated,omitempty"`
}

// Deliver compiles the target set, subtracts exclusions, identity-checks each
// selected lane, and prompts exactly once. Excluded lanes are never prompted.
func (d *Deliverer) Deliver(ctx context.Context, id, action, text string, candidates []Target) (Receipt, error) {
	if d == nil || d.Prompt == nil {
		return Receipt{}, errors.New("broadcast: prompt function is required")
	}
	if d.Live == nil {
		return Receipt{}, errors.New("broadcast: live identity function is required")
	}
	if strings.TrimSpace(id) == "" || strings.TrimSpace(action) == "" {
		return Receipt{}, errors.New("broadcast: id and action are required")
	}
	now := time.Now
	if d.Now != nil {
		now = d.Now
	}
	sel := Select(candidates)
	rec := Receipt{
		ID:       id,
		Action:   action,
		At:       now().UTC(),
		Excluded: sel.Excluded,
	}
	for _, t := range sel.Selected {
		tr := TargetReceipt{Target: t}
		live, err := d.Live(ctx, t)
		if err != nil {
			tr.Error = err.Error()
			rec.Selected = append(rec.Selected, tr)
			continue
		}
		// Force action onto both sides for the allowed-action gate.
		bound := IdentityFromTarget(t, action)
		live.Action = action
		if err := CheckIdentity(bound, live); err != nil {
			tr.Error = err.Error()
			rec.Selected = append(rec.Selected, tr)
			continue
		}
		if err := d.Prompt(ctx, t, text); err != nil {
			tr.Error = err.Error()
			rec.Selected = append(rec.Selected, tr)
			continue
		}
		tr.Prompted = true
		rec.Selected = append(rec.Selected, tr)
	}
	return rec, nil
}

// PromptedNames returns names that received a successful prompt (for tests).
func (r Receipt) PromptedNames() []string {
	var out []string
	for _, s := range r.Selected {
		if s.Prompted {
			out = append(out, s.Target.Name)
		}
	}
	return out
}

// ExcludedNames returns names that were excluded before delivery.
func (r Receipt) ExcludedNames() []string {
	out := make([]string, 0, len(r.Excluded))
	for _, e := range r.Excluded {
		out = append(out, e.Target.Name)
	}
	return out
}

// ReceiptStore appends durable broadcast receipts as JSONL.
type ReceiptStore struct {
	Path string
	mu   sync.Mutex
}

// Append persists one receipt. Fail-closed: empty path or marshal failure
// returns an error.
func (s *ReceiptStore) Append(r Receipt) error {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return errors.New("broadcast: receipt path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return fmt.Errorf("broadcast: create receipt dir: %w", err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("broadcast: marshal receipt: %w", err)
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("broadcast: open receipt: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("broadcast: write receipt: %w", err)
	}
	return f.Sync()
}

// RecordCompensation appends a compensation note onto a receipt and persists it.
func (s *ReceiptStore) RecordCompensation(r Receipt, reason string) (Receipt, error) {
	r.Compensated = append(append([]string(nil), r.Compensated...), reason)
	if err := s.Append(r); err != nil {
		return r, err
	}
	return r, nil
}
