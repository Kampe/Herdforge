// Package ledger defines the versioned Phase 1 contracts for Herdforge's
// private, local-operator lifecycle ledger. It intentionally defines data
// contracts only; transport and public authentication are out of scope.
package ledger

import (
	"encoding/json"
	"time"
)

const (
	// Version1 is the initial stable shape shared by the Go contracts and
	// PostgreSQL migration.
	Version1 = 1
	// SchemaName is owned by Herdforge. Cauldron owns its own logical schema
	// and is deliberately not created or mutated by this package.
	SchemaName = "herdforge"
)

type Actor struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	Kind        string    `json:"kind"`
	DisplayName string    `json:"display_name"`
	CreatedAt   time.Time `json:"created_at"`
}

func (Actor) ContractVersion() int { return Version1 }

type Principal struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	ActorID   string    `json:"actor_id"`
	Kind      string    `json:"kind"`
	Label     string    `json:"label"`
	CreatedAt time.Time `json:"created_at"`
}

func (Principal) ContractVersion() int { return Version1 }

type Run struct {
	Version     int        `json:"version"`
	ID          string     `json:"id"`
	Repository  string     `json:"repository"`
	BaseSHA     string     `json:"base_sha"`
	OwnerID     string     `json:"owner_id"`
	Status      string     `json:"status"`
	CreatedAt   time.Time  `json:"created_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

func (Run) ContractVersion() int { return Version1 }

type Phase struct {
	Version   int       `json:"version"`
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Ordinal   int       `json:"ordinal"`
	Kind      string    `json:"kind"`
	Status    string    `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func (Phase) ContractVersion() int { return Version1 }

type Candidate struct {
	Version        int       `json:"version"`
	ID             string    `json:"id"`
	RunID          string    `json:"run_id"`
	PhaseID        string    `json:"phase_id"`
	GitSHA         string    `json:"git_sha"`
	BaseSHA        string    `json:"base_sha"`
	EvidenceDigest string    `json:"evidence_digest"`
	CreatedAt      time.Time `json:"created_at"`
}

func (Candidate) ContractVersion() int { return Version1 }

type Receipt struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	CandidateID    string          `json:"candidate_id"`
	Kind           string          `json:"kind"`
	EvidenceDigest string          `json:"evidence_digest"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"created_at"`
}

func (Receipt) ContractVersion() int { return Version1 }

type Review struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	CandidateID string    `json:"candidate_id"`
	ReviewerID  string    `json:"reviewer_id"`
	Outcome     string    `json:"outcome"`
	ReceiptID   string    `json:"receipt_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func (Review) ContractVersion() int { return Version1 }

type Approval struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	CandidateID string    `json:"candidate_id"`
	ApproverID  string    `json:"approver_id"`
	Decision    string    `json:"decision"`
	ReceiptID   string    `json:"receipt_id"`
	CreatedAt   time.Time `json:"created_at"`
}

func (Approval) ContractVersion() int { return Version1 }

type SpendEntry struct {
	Version     int       `json:"version"`
	ID          string    `json:"id"`
	RunID       string    `json:"run_id"`
	ActorID     string    `json:"actor_id"`
	PrincipalID string    `json:"principal_id"`
	AmountUSD   string    `json:"amount_usd"`
	TokenCount  int64     `json:"token_count"`
	RecordedAt  time.Time `json:"recorded_at"`
}

func (SpendEntry) ContractVersion() int { return Version1 }

type OwnedWorktree struct {
	Version     int        `json:"version"`
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	CandidateID string     `json:"candidate_id,omitempty"`
	Path        string     `json:"path"`
	Branch      string     `json:"branch"`
	BaseSHA     string     `json:"base_sha"`
	OwnerID     string     `json:"owner_id"`
	CreatedAt   time.Time  `json:"created_at"`
	ReleasedAt  *time.Time `json:"released_at,omitempty"`
}

func (OwnedWorktree) ContractVersion() int { return Version1 }

type LifecycleEvent struct {
	Version        int             `json:"version"`
	ID             string          `json:"id"`
	RunID          string          `json:"run_id"`
	Sequence       int64           `json:"sequence"`
	Type           string          `json:"type"`
	ActorID        string          `json:"actor_id"`
	PrincipalID    string          `json:"principal_id"`
	PhaseID        string          `json:"phase_id,omitempty"`
	CandidateID    string          `json:"candidate_id,omitempty"`
	EvidenceDigest string          `json:"evidence_digest,omitempty"`
	Payload        json.RawMessage `json:"payload"`
	IdempotencyKey string          `json:"idempotency_key"`
	CreatedAt      time.Time       `json:"created_at"`
}

func (LifecycleEvent) ContractVersion() int { return Version1 }
