package reviewledger

import "time"

// Event types for the append-only ledger.
type Event string

const (
	EventRecord   Event = "record"
	EventVerdict  Event = "verdict"
	EventRepair   Event = "repair"
	EventConsumed Event = "consumed"
	EventEnqueue  Event = "enqueue"
	EventRevoked  Event = "revoked"
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
	Timestamp      string `json:"ts"`
	Event          string `json:"event"`
	SHA            string `json:"sha,omitempty"`
	Branch         string `json:"branch,omitempty"`
	BuilderFamily  string `json:"builder_family,omitempty"`
	ReviewerFamily string `json:"reviewer_family,omitempty"`
	Reviewer       string `json:"reviewer,omitempty"`
	Provider       string `json:"provider,omitempty"`
	Model          string `json:"model,omitempty"`
	Pane           string `json:"pane,omitempty"`
	Pid            string `json:"pid,omitempty"`
	Artifact       string `json:"artifact,omitempty"`
	Gate           string `json:"gate,omitempty"`
	Tier           string `json:"tier,omitempty"`
	Verdict        string `json:"verdict,omitempty"`
	RepairAuthor   string `json:"repair_author,omitempty"`
	RepairFamily   string `json:"repair_family,omitempty"`
	Lane           string `json:"lane,omitempty"`
	MergeSHA       string `json:"merge_sha,omitempty"`
	Status         string `json:"status,omitempty"`
}

// familyResolve represents the 3-state family resolution.
type familyResolve int

const (
	familyUnset    familyResolve = iota
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
	RepoRoot     string
	Path         string
	QueuePath    string
	Now          func() time.Time
	Coordinators map[string]struct{}
}
