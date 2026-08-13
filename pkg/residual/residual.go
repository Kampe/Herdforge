// Package residual makes deliberate incompleteness durable without turning it
// into completion authority. A residual is a hold and a follow-up obligation,
// never a waiver for verification or a required acceptance criterion.
package residual

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type Kind string

const (
	KindUnmetCriterion     Kind = "unmet_criterion"
	KindDeferredFunction   Kind = "deferred_functionality"
	KindExternalDependency Kind = "blocked_external_dependency"
)

type Severity string

const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
)

var (
	ErrInvalid        = errors.New("residual: invalid record")
	ErrMissingLinkage = errors.New("residual: follow-up linkage evidence missing")
	ErrRequired       = errors.New("residual: required acceptance criterion remains unmet")
)

// Record is an immutable, revision-bound statement of remaining work. ID is
// deterministic over every authority-bearing field; callers create a new
// record rather than mutating one after the board revision changes.
type Record struct {
	ID                 string   `json:"id"`
	Kind               Kind     `json:"kind"`
	Severity           Severity `json:"severity"`
	Rationale          string   `json:"rationale"`
	TaskID             string   `json:"task_id"`
	TaskRef            string   `json:"task_ref"`
	AcceptanceRevision string   `json:"acceptance_revision"`
	EvidenceRef        string   `json:"evidence_ref"`
	RequiredCriterion  bool     `json:"required_criterion"`
	FollowUpTaskID     string   `json:"follow_up_task_id,omitempty"`
	FollowUpRef        string   `json:"follow_up_ref,omitempty"`
	LinkEvidence       string   `json:"link_evidence,omitempty"`
}

func New(kind Kind, severity Severity, rationale, taskID, taskRef, revision, evidence string, required bool) (Record, error) {
	r := Record{Kind: kind, Severity: severity, Rationale: strings.TrimSpace(rationale), TaskID: strings.TrimSpace(taskID), TaskRef: strings.TrimSpace(taskRef), AcceptanceRevision: strings.TrimSpace(revision), EvidenceRef: strings.TrimSpace(evidence), RequiredCriterion: required}
	r.ID = identity(r)
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	return r, nil
}

func (r Record) Validate() error {
	if r.Kind != KindUnmetCriterion && r.Kind != KindDeferredFunction && r.Kind != KindExternalDependency {
		return fmt.Errorf("%w: unknown kind", ErrInvalid)
	}
	if r.Severity != SeverityLow && r.Severity != SeverityMedium && r.Severity != SeverityHigh {
		return fmt.Errorf("%w: unknown severity", ErrInvalid)
	}
	if r.Rationale == "" || r.TaskID == "" || r.TaskRef == "" || r.AcceptanceRevision == "" || r.EvidenceRef == "" {
		return fmt.Errorf("%w: rationale, task identity, revision, and evidence are required", ErrInvalid)
	}
	if r.ID == "" || r.ID != identity(r) {
		return fmt.Errorf("%w: record identity does not bind immutable fields", ErrInvalid)
	}
	return nil
}

func identity(r Record) string {
	h := sha256.Sum256([]byte(strings.Join([]string{string(r.Kind), string(r.Severity), r.Rationale, r.TaskID, r.TaskRef, r.AcceptanceRevision, r.EvidenceRef, fmt.Sprintf("%t", r.RequiredCriterion)}, "\x00")))
	return hex.EncodeToString(h[:])
}

// FollowUp is the minimal configured-provider read/write capability required
// for scope-reduced exits. Find must return only already-linked follow-ups for
// the same immutable record ID; Create must attach the evidence reference.
type FollowUp struct{ ID, Ref, ResidualID, EvidenceRef string }
type Provider interface {
	FindResidualFollowUps(context.Context, string, string) ([]FollowUp, error)
	CreateResidualFollowUp(context.Context, Record) (FollowUp, error)
	ReadResidualFollowUp(context.Context, string) (FollowUp, error)
}

// EnsureFollowUp creates at most one linked follow-up and then independently
// reads it back. Ambiguity, a stale/mismatched record, or missing evidence is
// a hard error. The returned Record is a new immutable projection carrying
// linkage evidence; the input is never changed.
func EnsureFollowUp(ctx context.Context, p Provider, r Record) (Record, error) {
	if err := r.Validate(); err != nil {
		return Record{}, err
	}
	if p == nil {
		return Record{}, fmt.Errorf("%w: configured provider lacks residual follow-up capability", ErrMissingLinkage)
	}
	existing, err := p.FindResidualFollowUps(ctx, r.TaskID, r.ID)
	if err != nil {
		return Record{}, fmt.Errorf("%w: list linked follow-ups: %v", ErrMissingLinkage, err)
	}
	if len(existing) > 1 {
		return Record{}, fmt.Errorf("%w: multiple linked follow-ups for residual %s", ErrMissingLinkage, r.ID)
	}
	var linked FollowUp
	if len(existing) == 1 {
		linked = existing[0]
	} else {
		linked, err = p.CreateResidualFollowUp(ctx, r)
		if err != nil {
			return Record{}, fmt.Errorf("%w: create linked follow-up: %v", ErrMissingLinkage, err)
		}
	}
	if strings.TrimSpace(linked.ID) == "" {
		return Record{}, fmt.Errorf("%w: provider returned follow-up without identity", ErrMissingLinkage)
	}
	read, err := p.ReadResidualFollowUp(ctx, linked.ID)
	if err != nil {
		return Record{}, fmt.Errorf("%w: read back linked follow-up: %v", ErrMissingLinkage, err)
	}
	if read.ID != linked.ID || read.ResidualID != r.ID || read.EvidenceRef != r.EvidenceRef || strings.TrimSpace(read.Ref) == "" {
		return Record{}, fmt.Errorf("%w: provider readback does not bind residual and evidence", ErrMissingLinkage)
	}
	r.FollowUpTaskID, r.FollowUpRef = read.ID, read.Ref
	r.LinkEvidence = "provider-readback:" + read.ID + ":" + r.EvidenceRef
	return r, nil
}

// ValidateExit is the final nonterminal scope-reduction gate. Required
// acceptance work remains a hard veto even when its follow-up is linked.
func ValidateExit(records []Record, revision string) error {
	for _, r := range records {
		if err := r.Validate(); err != nil {
			return err
		}
		if r.AcceptanceRevision != strings.TrimSpace(revision) {
			return fmt.Errorf("%w: residual %s is bound to another acceptance revision", ErrMissingLinkage, r.ID)
		}
		if r.RequiredCriterion {
			return fmt.Errorf("%w: %s", ErrRequired, r.ID)
		}
		if r.FollowUpTaskID == "" || r.FollowUpRef == "" || r.LinkEvidence != "provider-readback:"+r.FollowUpTaskID+":"+r.EvidenceRef {
			return fmt.Errorf("%w: %s", ErrMissingLinkage, r.ID)
		}
	}
	return nil
}

// PacketSection is stable, deterministic downstream context for workers.
func PacketSection(records []Record) (string, error) {
	copyRecords := append([]Record(nil), records...)
	sort.Slice(copyRecords, func(i, j int) bool { return copyRecords[i].ID < copyRecords[j].ID })
	var b strings.Builder
	for _, r := range copyRecords {
		if err := r.Validate(); err != nil {
			return "", err
		}
		fmt.Fprintf(&b, "RESIDUAL id=%s kind=%s severity=%s task=%s revision=%s evidence=%s follow_up=%s required=%t\n", r.ID, r.Kind, r.Severity, r.TaskRef, r.AcceptanceRevision, r.EvidenceRef, r.FollowUpRef, r.RequiredCriterion)
	}
	return b.String(), nil
}
