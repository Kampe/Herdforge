package reviewledger

import (
	"sync"
	"time"
)

// Event types for the append-only ledger.
type Event string

const (
	EventRecord       Event = "record"
	EventVerdict      Event = "verdict"
	EventRepair       Event = "repair"
	EventConsumed     Event = "consumed"
	EventEnqueue      Event = "enqueue"
	EventRevoked      Event = "revoked"
	EventRefutation   Event = "refutation"
	EventSupersession Event = "supersession"
	// EventReconstruction records an operator-attested replacement identity
	// used when a reviewed candidate was harvested by content-preserving
	// reconstruction rather than literal ancestry.
	EventReconstruction Event = "reconstruction"
	EventRetired        Event = "retired"
)

// Verdict values.
type Verdict string

const (
	VerdictPASS    Verdict = "PASS"
	VerdictFAIL    Verdict = "FAIL"
	VerdictBLOCKED Verdict = "BLOCKED"
)

// FamilyAllowlist defines the 11 known builder families.
var FamilyAllowlist = map[string]bool{
	"anthropic":   true,
	"openai":      true,
	"google":      true,
	"xai":         true,
	"zhipu":       true,
	"moonshot":    true,
	"alibaba":     true,
	"deepseek":    true,
	"open-weight": true,
	"antigravity": true,
	"proxy":       true,
}

// LedgerRow matches the jq-emitted JSONL row exactly (JSON tags for jq compat).
type LedgerRow struct {
	Timestamp          string `json:"ts"`
	Event              string `json:"event"`
	SHA                string `json:"sha,omitempty"`
	Branch             string `json:"branch,omitempty"`
	BuilderFamily      string `json:"builder_family,omitempty"`
	BuilderIdentity    string `json:"builder_identity,omitempty"`
	ReviewerFamily     string `json:"reviewer_family,omitempty"`
	Reviewer           string `json:"reviewer,omitempty"`
	Provider           string `json:"provider,omitempty"`
	Model              string `json:"model,omitempty"`
	Pane               string `json:"pane,omitempty"`
	Pid                string `json:"pid,omitempty"`
	Artifact           string `json:"artifact,omitempty"`
	Gate               string `json:"gate,omitempty"`
	Tier               string `json:"tier,omitempty"`
	Verdict            string `json:"verdict,omitempty"`
	RepairAuthor       string `json:"repair_author,omitempty"`
	RepairFamily       string `json:"repair_family,omitempty"`
	Lane               string `json:"lane,omitempty"`
	MergeSHA           string `json:"merge_sha,omitempty"`
	Status             string `json:"status,omitempty"`
	Task               string `json:"task,omitempty"`
	Lease              string `json:"lease,omitempty"`
	PatchURL           string `json:"patch_url,omitempty"`
	VerificationDigest string `json:"verification_digest,omitempty"`
	FindingsRef        string `json:"findings_ref,omitempty"`
	CandidateSHA       string `json:"candidate_sha,omitempty"`
	RetryOf            string `json:"retry_of,omitempty"`
	Reason             string `json:"reason,omitempty"`
	ContentProof       string `json:"content_proof,omitempty"`
	Authority          string `json:"authority,omitempty"`
}

// familyResolve represents the 3-state family resolution.
type familyResolve int

const (
	familyUnset familyResolve = iota
	familySet
	familyConflict
)

// familyState pairs the resolved value with its conflict status.
type familyState struct {
	Value string
	State familyResolve
}

// Coordinators defines the default coordinator names.
var DefaultCoordinators = map[string]struct{}{
	"chainseer-orchestrator": {},
	"coordinator":            {},
}

// Ledger is an append-only JSONL review-verdict ledger with a harvest queue.
type Ledger struct {
	mu           sync.Mutex
	RepoRoot     string
	Path         string
	QueuePath    string
	Now          func() time.Time
	Coordinators map[string]struct{}
}
