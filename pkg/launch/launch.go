// Package launch owns the fail-closed boundary between routing decisions and
// write-capable worker processes.
package launch

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/toolpolicy"
)

const (
	WorkerRole           = "worker"
	ForgeSmithRole       = "forge-smith"
	RecoveryRole         = "recovery"
	OrchestratorRole     = "orchestrator"
	ReviewerRole         = "reviewer"
	AssayerRole          = "assayer"
	ScoutPlannerRole     = "scout-planner"
	VerificationGateRole = "verification-gate"
	ReviewSupervisorRole = "review-supervisor"
	HarvestRole          = "harvest"
	RecoverySentinelRole = "recovery-sentinel"
	Implementation       = "implementation"
)

// Request is the complete, durable identity of one process launch.
type Request struct {
	Decision        *router.LaunchDecision
	TaskRef         string
	Name            string
	TabID           string
	PaneID          string
	HerdrSession    string
	CWD             string
	ProcessIdentity string
	StartToken      string
	LeaseGeneration int64
	// SessionGeneration fences the Herdr session independently of task lease
	// generations; lane launches intentionally have LeaseGeneration == 0.
	SessionGeneration int64
	Scope             string
	Repository        string
	Lane              string
	PacketDigest      string
}

// Receipt is durable evidence for one launch attempt. Validation does not
// write acceptance: acceptance is recorded only after the process API starts.
type Receipt struct {
	CreatedAt         time.Time `json:"created_at"`
	TaskRef           string    `json:"task_ref,omitempty"`
	Role              string    `json:"role"`
	TaskShape         string    `json:"task_shape"`
	Provider          string    `json:"provider"`
	Model             string    `json:"model"`
	Effort            string    `json:"effort"`
	DecisionDigest    string    `json:"decision_digest"`
	Argv              []string  `json:"argv,omitempty"`
	Accepted          bool      `json:"accepted"`
	Reason            string    `json:"reason,omitempty"`
	Name              string    `json:"name,omitempty"`
	PaneID            string    `json:"pane_id,omitempty"`
	LeaseGeneration   int64     `json:"lease_generation,omitempty"`
	SessionGeneration int64     `json:"session_generation,omitempty"`
	Repository        string    `json:"repository,omitempty"`
	Lane              string    `json:"lane,omitempty"`
	Generation        int64     `json:"generation,omitempty"`
	TabID             string    `json:"tab_id,omitempty"`
	HerdrSession      string    `json:"herdr_session,omitempty"`
	CWD               string    `json:"cwd,omitempty"`
	ProcessIdentity   string    `json:"process_identity,omitempty"`
	StartToken        string    `json:"start_token,omitempty"`
	PacketDigest      string    `json:"packet_digest,omitempty"`
}

// Sink makes receipt durability injectable without making process tests touch
// the host filesystem.
type Sink interface{ Write(Receipt) error }

type JSONLSink struct {
	Path string
	mu   sync.Mutex
}

func (s *JSONLSink) Write(r Receipt) error {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return fmt.Errorf("launch receipt path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0755); err != nil {
		return fmt.Errorf("create launch receipt directory: %w", err)
	}
	f, err := os.OpenFile(s.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return fmt.Errorf("open launch receipt: %w", err)
	}
	defer f.Close()
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("marshal launch receipt: %w", err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write launch receipt: %w", err)
	}
	return f.Sync()
}

type MemorySink struct {
	mu       sync.Mutex
	Receipts []Receipt
}

func (s *MemorySink) Write(r Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Receipts = append(s.Receipts, r)
	return nil
}

func DefaultSink() Sink {
	p := os.Getenv("HERD_LAUNCH_RECEIPTS")
	if p == "" {
		p = ".herd/launch-receipts.jsonl"
	}
	return &JSONLSink{Path: p}
}

func normalized(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	v = strings.ReplaceAll(v, "_", "-")
	for _, prefix := range []string{"codex/", "openai/", "litellm/codex/", "litellm/openai/"} {
		v = strings.TrimPrefix(v, prefix)
	}
	return v
}

func clone(v []string) []string { return append([]string(nil), v...) }

func DecisionDigest(d *router.LaunchDecision) string {
	if d == nil {
		return ""
	}
	b, _ := json.Marshal(d)
	return fmt.Sprintf("sha256:%x", sha256.Sum256(b))
}

func fields(req Request) (role, shape, provider, model, effort, digest string, argv []string) {
	if req.Decision == nil {
		return "", "", "", "", "", "", nil
	}
	d := req.Decision
	return string(d.Role), d.Shape, d.Provider, d.Model, d.Effort, DecisionDigest(d), clone(d.Argv)
}

func reject(req Request, sink Sink, reason string) error {
	if sink == nil {
		sink = DefaultSink()
	}
	err := fmt.Errorf("launch rejected: %s", reason)
	role, shape, provider, model, effort, digest, argv := fields(req)
	if werr := sink.Write(Receipt{CreatedAt: time.Now().UTC(), TaskRef: req.TaskRef, Role: role, TaskShape: shape, Provider: provider, Model: model, Effort: effort, DecisionDigest: digest, Argv: argv, Reason: reason, Name: req.Name, PaneID: req.PaneID, LeaseGeneration: req.LeaseGeneration, SessionGeneration: req.SessionGeneration}); werr != nil {
		return fmt.Errorf("%w; failed to write failed-launch receipt: %v", err, werr)
	}
	return err
}

// RecordStarted is the single acceptance receipt for a process launch.
func RecordStarted(req Request, sink Sink) error {
	if sink == nil {
		sink = DefaultSink()
	}
	role, shape, provider, model, effort, digest, argv := fields(req)
	return sink.Write(Receipt{CreatedAt: time.Now().UTC(), TaskRef: req.TaskRef, Role: role, TaskShape: shape, Provider: provider, Model: model, Effort: effort, DecisionDigest: digest, Argv: argv, Accepted: true, Reason: "process started", Name: req.Name, PaneID: req.PaneID, LeaseGeneration: req.LeaseGeneration, SessionGeneration: req.SessionGeneration})
}

func RecordRejected(req Request, sink Sink, reason string) error { return reject(req, sink, reason) }

// HasStarted proves a resumable identity from the client-owned lifecycle
// store; Herdr's agent list does not carry routing metadata.
func HasStarted(req Request) (bool, error) {
	if req.Decision == nil || req.TaskRef == "" || req.Name == "" || req.PaneID == "" {
		return false, nil
	}
	p := os.Getenv("HERD_LAUNCH_RECEIPTS")
	if p == "" {
		p = ".herd/launch-receipts.jsonl"
	}
	f, err := os.Open(p)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	role, shape, provider, model, effort, digest, argv := fields(req)
	role, shape, provider, model, effort = normalized(role), normalized(shape), normalized(provider), normalized(model), normalized(effort)
	matched := false
	invalidated := false
	dec := json.NewDecoder(f)
	for {
		var r Receipt
		if err := dec.Decode(&r); err != nil {
			if err == io.EOF {
				break
			}
			return false, err
		}
		if r.TaskRef != req.TaskRef || r.Name != req.Name || r.PaneID != req.PaneID || r.LeaseGeneration != req.LeaseGeneration || r.SessionGeneration != req.SessionGeneration {
			continue
		}
		// Every record for this session generation participates in the decision:
		// a rejection or conflicting accepted record must not leave an older match
		// resumable.
		if !r.Accepted {
			invalidated = true
			continue
		}
		receiptTuplePresent := r.Role != "" && r.TaskShape != "" && r.Provider != "" && r.Model != "" && r.Effort != "" && len(r.Argv) > 0
		receiptTupleMatches := receiptTuplePresent && normalized(r.Role) == role && normalized(r.TaskShape) == shape && normalized(r.Provider) == provider && normalized(r.Model) == model && normalized(r.Effort) == effort && r.DecisionDigest == digest && equalStrings(r.Argv, argv)
		if !receiptTupleMatches {
			invalidated = true
			continue
		}
		matched = true
	}
	if req.SessionGeneration <= 0 {
		return false, nil
	}
	return matched && !invalidated, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Validate checks all launch identity fields before any tab, process, prompt,
// board, or worktree operation. Worker/recovery aliases are deliberately
// narrow: coordinator-only models can never enter a worker boundary.
func Validate(req Request, sink Sink) error {
	role, shape, provider, model, effort, _, argv := fields(req)
	if role == "" || shape == "" || provider == "" || model == "" || effort == "" || len(argv) == 0 {
		return reject(req, sink, "an actual compiled LaunchDecision with role, task shape, provider, model, effort, and argv is required")
	}
	if err := router.VerifyDecisionForScope(req.Decision, req.TaskRef, req.LeaseGeneration, req.Scope); err != nil {
		return reject(req, sink, err.Error())
	}
	role, shape, provider, model, effort = normalized(role), normalized(shape), normalized(provider), normalized(model), normalized(effort)
	validShape := map[string]bool{"coordinator": true, "architecture": true, "implementation": true, "research": true, "bounded": true, "advisory": true, "qa-light": true, "qa": true, "adversarial": true}
	if !validShape[shape] {
		return reject(req, sink, "unknown task shape")
	}
	worker := role == WorkerRole || role == ForgeSmithRole || role == RecoveryRole
	if worker {
		if shape == "coordinator" {
			return reject(req, sink, "coordinator task shape is not valid for worker launch")
		}
		if shape != Implementation {
			return reject(req, sink, "worker task shape must be implementation")
		}
		// No vendor tuple: the routed decision already came from the live
		// quota-ranked implementation waterfall. Validate that it is coherent,
		// not that it names one preordained provider.
		if strings.TrimSpace(provider) == "" || strings.TrimSpace(model) == "" || strings.TrimSpace(effort) == "" {
			return reject(req, sink, "worker launch decision is missing provider, model, or effort")
		}
	} else if !controlRole(role) {
		return reject(req, sink, "unknown launch role")
	}
	want := clone(argv)
	if worker {
		// Derive the expected argv from the provider contract rather than
		// re-spelling one vendor's command line here; the literal silently
		// pinned every worker launch to codex.
		want = router.ArgvFor(provider, model, effort)
		if len(want) == 0 {
			return reject(req, sink, fmt.Sprintf("no launch argv contract for provider %q", provider))
		}
	}
	if !worker && (len(want) < 2 || want[0] == "" || want[1] != "--model" || !argvCarriesEffort(provider, want, effort)) {
		return reject(req, sink, "non-worker launch argv must explicitly carry --model and effort")
	}
	if len(argv) != len(want) {
		return reject(req, sink, fmt.Sprintf("argv must match the routed launch contract: want %v, got %v", want, argv))
	}
	for i := range want {
		if argv[i] != want[i] {
			return reject(req, sink, fmt.Sprintf("argv[%d]=%q does not match routed launch decision", i, argv[i]))
		}
	}
	// CRG MCP isolation is a codex-argv concern (-c mcp_servers…=false). This
	// read `provider == WorkerProvider`, which only meant "codex" while the
	// worker tuple happened to be codex; once the worker moved to grok it
	// demanded a codex-only guarantee from a provider that has no such flag.
	// Non-codex providers were never covered by this check.
	if strings.EqualFold(provider, "codex") {
		compiled, cfg, err := toolpolicy.Require(toolpolicy.Role(role), provider, argv)
		if err != nil || !cfg.Valid() || !equalStrings(compiled, argv) {
			return reject(req, sink, "codex launch lacks explicit CRG MCP isolation")
		}
	}
	modelIndex := -1
	for i := range argv {
		if argv[i] == "--model" && i+1 < len(argv) {
			modelIndex = i + 1
			break
		}
	}
	if modelIndex < 0 || normalized(argv[modelIndex]) != model {
		return reject(req, sink, "argv model does not match the routed decision")
	}
	return nil
}

func argvCarriesEffort(provider string, argv []string, effort string) bool {
	joined := strings.Join(argv, " ")
	switch normalized(provider) {
	case "codex":
		return strings.Contains(joined, "model_reasoning_effort="+effort)
	case "claude":
		return strings.Contains(joined, "--effort "+effort)
	case "grok":
		return strings.Contains(joined, "--reasoning-effort "+effort)
	default:
		// Some harnesses (kimi/opencode) have no native effort flag; their
		// routed argv remains authoritative through model/provider binding.
		return true
	}
}

func controlRole(role string) bool {
	switch role {
	case OrchestratorRole, ReviewerRole, AssayerRole, ScoutPlannerRole,
		VerificationGateRole, ReviewSupervisorRole, HarvestRole, RecoverySentinelRole:
		return true
	default:
		return false
	}
}
