// Package launch owns the fail-closed boundary between routing decisions and
// write-capable worker processes.
package launch

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
	"github.com/Kampe/Herdforge/pkg/harness"
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
	WorkerProvider       = "codex"
	WorkerModel          = "gpt-5.6-luna"
	WorkerEffort         = "medium"
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
	// FleetBinding is the authenticated fleet-execution contract (FAC-173).
	// When set, Validate and recovery re-proof require it to match the live
	// lease generation. Nested-agent argv denials are always required for
	// codex/claude regardless of whether a binding is present.
	FleetBinding       agentpolicy.LaunchBinding
	Hooks              []harness.Hook
	HookClient         *http.Client
	HookWarning        func(string)
	HookDiscovery      harness.HookDiscovery
	HookPolicyRevision string
}

// LaunchEffects are the write-capable operations that must remain behind the
// hook and routing preflight boundary. The callbacks are deliberately injected
// so the boundary can be proven without tabs, processes, prompts, or boards.
type LaunchEffects struct {
	Tab     func() error
	Process func() error
	Prompt  func() error
	Board   func() error
}

// Launch validates the complete launch contract before invoking any
// write-capable collaborator. Required hook health is checked first, so a
// rejected launch emits only its stable hook receipt and reaches no side effect.
func Launch(req Request, sink Sink, effects LaunchEffects) error {
	if err := Validate(req, sink); err != nil {
		return err
	}
	for _, effect := range []struct {
		name string
		fn   func() error
	}{
		{name: "tab", fn: effects.Tab},
		{name: "process", fn: effects.Process},
		{name: "prompt", fn: effects.Prompt},
		{name: "board", fn: effects.Board},
	} {
		if effect.fn == nil {
			continue
		}
		if err := effect.fn(); err != nil {
			return fmt.Errorf("launch %s side effect: %w", effect.name, err)
		}
	}
	return nil
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
	// Fleet-execution contract fields (FAC-173). Independent of checkout-local
	// AGENTS files; recovery re-reads these with the exact lease generation.
	FleetPolicyDigest string `json:"fleet_policy_digest,omitempty"`
	FleetAuthTag      string `json:"fleet_auth_tag,omitempty"`
	FleetFamily       string `json:"fleet_parent_family,omitempty"`
	FleetSurface      string `json:"fleet_allowed_surface,omitempty"`
	// Hook preflight evidence (FAC-177).
	PolicyRevision    string `json:"policy_revision,omitempty"`
	Kind              string `json:"kind,omitempty"`
	HookCode          string `json:"hook_code,omitempty"`
	HookName          string `json:"hook_name,omitempty"`
	EndpointClass     string `json:"endpoint_class,omitempty"`
	RedactedAuthority string `json:"redacted_authority,omitempty"`
	ReceiptKey        string `json:"receipt_key,omitempty"`
}

type DegradedStatus struct {
	TaskRef           string
	LeaseGeneration   int64
	PolicyRevision    string
	DecisionDigest    string
	HookCode          string
	HookName          string
	EndpointClass     string
	RedactedAuthority string
}

// ReadDegradedStatus projects durable optional-hook receipts for status
// consumers without claiming process acceptance or revocation semantics.
func ReadDegradedStatus(path string) ([]DegradedStatus, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	type lifecycle struct {
		status DegradedStatus
		kind   string
	}
	lifecycles := make(map[string]lifecycle)
	order := make([]string, 0)
	dec := json.NewDecoder(f)
	for {
		var receipt Receipt
		if err := dec.Decode(&receipt); err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		if receipt.Kind != "hook_degraded" && receipt.Kind != "hook_recovered" {
			continue
		}
		key := fmt.Sprintf("%s|%d|%s|%s", receipt.TaskRef, receipt.LeaseGeneration, receipt.PolicyRevision, receipt.DecisionDigest)
		if _, exists := lifecycles[key]; !exists {
			order = append(order, key)
		}
		lifecycles[key] = lifecycle{status: DegradedStatus{TaskRef: receipt.TaskRef, LeaseGeneration: receipt.LeaseGeneration, PolicyRevision: receipt.PolicyRevision, DecisionDigest: receipt.DecisionDigest, HookCode: receipt.HookCode, HookName: receipt.HookName, EndpointClass: receipt.EndpointClass, RedactedAuthority: receipt.RedactedAuthority}, kind: receipt.Kind}
	}
	result := make([]DegradedStatus, 0, len(order))
	for _, key := range order {
		entry := lifecycles[key]
		if entry.kind == "hook_degraded" {
			result = append(result, entry.status)
		}
	}
	return result, nil
}

// Sink makes receipt durability injectable without making process tests touch
// the host filesystem.
type Sink interface{ Write(Receipt) error }

type deduplicatingSink interface {
	Sink
	WriteOnce(Receipt) (bool, error)
}

type degradedHistory interface {
	HasDegraded(Request) (bool, error)
}

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
	return s.withFileLock(func() error {
		return s.appendLocked(r)
	})
}

func (s *JSONLSink) withFileLock(fn func() error) error {
	lock, err := os.OpenFile(s.Path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open launch receipt lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock launch receipts: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	return fn()
}

func (s *JSONLSink) appendLocked(r Receipt) error {
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

func (s *JSONLSink) WriteOnce(r Receipt) (bool, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return false, fmt.Errorf("launch receipt path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(filepath.Dir(s.Path), 0755); err != nil {
		return false, fmt.Errorf("create launch receipt directory: %w", err)
	}
	written := false
	err := s.withFileLock(func() error {
		if r.ReceiptKey != "" {
			f, err := os.Open(s.Path)
			if err == nil {
				defer f.Close()
				dec := json.NewDecoder(f)
				for {
					var existing Receipt
					if err := dec.Decode(&existing); err != nil {
						if err == io.EOF {
							break
						}
						return fmt.Errorf("read launch receipts: %w", err)
					}
					if existing.ReceiptKey == r.ReceiptKey {
						return nil
					}
				}
			} else if !os.IsNotExist(err) {
				return fmt.Errorf("open launch receipts: %w", err)
			}
		}
		if err := s.appendLocked(r); err != nil {
			return err
		}
		written = true
		return nil
	})
	return written, err
}

func (s *JSONLSink) HasDegraded(req Request) (bool, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return false, fmt.Errorf("launch receipt path is required")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := false
	err := s.withFileLock(func() error {
		f, err := os.Open(s.Path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		defer f.Close()
		dec := json.NewDecoder(f)
		for {
			var receipt Receipt
			if err := dec.Decode(&receipt); err != nil {
				if err == io.EOF {
					return nil
				}
				return err
			}
			if receipt.Kind == "hook_degraded" && receipt.TaskRef == req.TaskRef && receipt.LeaseGeneration == req.LeaseGeneration && receipt.DecisionDigest == DecisionDigest(req.Decision) {
				found = true
				return nil
			}
		}
	})
	return found, err
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

func (s *MemorySink) WriteOnce(r Receipt) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.ReceiptKey != "" {
		for _, existing := range s.Receipts {
			if existing.ReceiptKey == r.ReceiptKey {
				return false, nil
			}
		}
	}
	s.Receipts = append(s.Receipts, r)
	return true, nil
}

func (s *MemorySink) HasDegraded(req Request) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	digest := DecisionDigest(req.Decision)
	for _, receipt := range s.Receipts {
		if receipt.Kind == "hook_degraded" && receipt.TaskRef == req.TaskRef && receipt.LeaseGeneration == req.LeaseGeneration && receipt.DecisionDigest == digest {
			return true, nil
		}
	}
	return false, nil
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

func fleetReceiptFields(req Request) (digest, auth, family, surface string) {
	b := req.FleetBinding
	return b.PolicyDigest, b.AuthTag, b.ParentExecutionFamily, b.AllowedHerdrSurface
}

func reject(req Request, sink Sink, reason string) error {
	if sink == nil {
		sink = DefaultSink()
	}
	err := fmt.Errorf("launch rejected: %s", reason)
	role, shape, provider, model, effort, digest, argv := fields(req)
	fd, fa, ff, fs := fleetReceiptFields(req)
	if werr := sink.Write(receiptFrom(req, role, shape, provider, model, effort, digest, argv, false, reason, fd, fa, ff, fs)); werr != nil {
		return fmt.Errorf("%w; failed to write failed-launch receipt: %v", err, werr)
	}
	return err
}

func receiptFrom(req Request, role, shape, provider, model, effort, digest string, argv []string, accepted bool, reason, fd, fa, ff, fs string) Receipt {
	return Receipt{
		CreatedAt: time.Now().UTC(), TaskRef: req.TaskRef, Role: role, TaskShape: shape,
		Provider: provider, Model: model, Effort: effort, DecisionDigest: digest, Argv: argv,
		Accepted: accepted, Reason: reason, Name: req.Name, PaneID: req.PaneID,
		LeaseGeneration: req.LeaseGeneration, SessionGeneration: req.SessionGeneration,
		Repository: req.Repository, Lane: req.Lane, TabID: req.TabID, HerdrSession: req.HerdrSession,
		CWD: req.CWD, ProcessIdentity: req.ProcessIdentity, StartToken: req.StartToken,
		PacketDigest: req.PacketDigest, FleetPolicyDigest: fd, FleetAuthTag: fa, FleetFamily: ff, FleetSurface: fs,
	}
}

// RecordStarted is the single acceptance receipt for a process launch.
func RecordStarted(req Request, sink Sink) error {
	if sink == nil {
		sink = DefaultSink()
	}
	role, shape, provider, model, effort, digest, argv := fields(req)
	policyRevision, err := currentPolicyRevision(req)
	if err != nil {
		return err
	}
	fd, fa, ff, fs := fleetReceiptFields(req)
	r := receiptFrom(req, role, shape, provider, model, effort, digest, argv, true, "process started", fd, fa, ff, fs)
	r.Kind = "process_started"
	r.PolicyRevision = policyRevision
	return sink.Write(r)
}

func currentPolicyRevision(req Request) (string, error) {
	if req.Decision == nil {
		return "", nil
	}
	discovery := req.HookDiscovery
	if discovery == nil {
		discovery = harness.DefaultDiscovery{}
	}
	result, err := discovery.Discover(req.Decision.Provider)
	if err != nil || result.State == harness.DiscoveryFailed || result.State == harness.DiscoveryNotDiscovered {
		return "", fmt.Errorf("launch hook policy discovery failed")
	}
	return result.PolicyRevision, nil
}

func RecordRejected(req Request, sink Sink, reason string) error { return reject(req, sink, reason) }

func writeOnce(sink Sink, receipt Receipt) (bool, error) {
	if sink == nil {
		sink = DefaultSink()
	}
	if dedupe, ok := sink.(deduplicatingSink); ok {
		return dedupe.WriteOnce(receipt)
	}
	if err := sink.Write(receipt); err != nil {
		return false, err
	}
	return true, nil
}

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
	currentRevision, err := currentPolicyRevision(req)
	if err != nil {
		return false, err
	}
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
		if r.TaskRef == req.TaskRef && r.LeaseGeneration == req.LeaseGeneration && r.PolicyRevision == currentRevision && r.DecisionDigest == digest && (r.Kind == "hook_degraded" || r.Kind == "launch_rejected") {
			if r.Kind == "hook_degraded" {
				continue
			}
			invalidated = true
			continue
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
		receiptTupleMatches = receiptTupleMatches && r.PolicyRevision == currentRevision
		if !receiptTupleMatches {
			invalidated = true
			continue
		}
		matched = true
	}
	if req.SessionGeneration <= 0 {
		return false, nil
	}
	if !matched || invalidated {
		return false, nil
	}
	if _, err := PreflightHooks(req); err != nil {
		return false, err
	}
	return true, nil
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
	if req.Decision == nil || role == "" || shape == "" || provider == "" || model == "" || effort == "" || len(argv) == 0 || strings.TrimSpace(req.Decision.Harness) == "" || len(req.Decision.HarnessArgv) == 0 {
		return reject(req, sink, "an actual compiled LaunchDecision with role, task shape, provider, model, effort, and argv is required")
	}
	if err := router.VerifyDecisionForScope(req.Decision, req.TaskRef, req.LeaseGeneration, req.Scope); err != nil {
		return reject(req, sink, err.Error())
	}
	wantHarness, wantHarnessArgv, err := router.HarnessArgvFor(provider, model, effort)
	if err != nil {
		return reject(req, sink, "launch harness does not match routed provider/model/effort")
	}
	if req.Decision.HarnessSession != "" {
		sessionPath := filepath.Clean(strings.TrimSpace(req.Decision.HarnessSession))
		if sessionPath == "." || !filepath.IsAbs(sessionPath) {
			return reject(req, sink, "launch harness session path is not absolute")
		}
		wantHarnessArgv = append(wantHarnessArgv, "--session", sessionPath)
	}
	if normalized(req.Decision.Harness) != normalized(wantHarness) || !equalStrings(req.Decision.HarnessArgv, wantHarnessArgv) {
		return reject(req, sink, "launch harness does not match routed provider/model/effort/session")
	}
	if _, err := preflightHooks(req, sink); err != nil {
		return err
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
	// --model must be PRESENT and carry a value; it need not be argv[1]. It was
	// pinned to index 1, which broke the moment the nested-agent denial work
	// prepended "--disable multi_agent --disable multi_agent_v2" to the codex
	// contract: every non-worker launch was then rejected as "missing --model"
	// while the flag was sitting three positions later. Position is a property
	// of one vendor's command line; presence is the property this gate is about.
	if !worker && (len(want) < 2 || want[0] == "" || !argvCarriesModel(want) || !argvCarriesEffort(provider, want, effort)) {
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
	// FAC-173: nested Claude/Codex collaboration tools must be compiled out
	// of the launch argv before any process starts. Prompt wording is not
	// an execution control.
	if err := agentpolicy.RequireNestedDeny(provider, argv); err != nil {
		return reject(req, sink, "launch lacks nested-agent denial controls")
	}
	// When a fleet-execution binding is presented, it must authenticate and
	// match the exact live lease generation. Callers that mint bindings
	// (dispatch/recovery) must not proceed with a stale or absent contract.
	if req.FleetBinding.PolicyDigest != "" || req.FleetBinding.AuthTag != "" {
		key, keyErr := agentpolicy.KeyFromEnv()
		if keyErr != nil {
			return reject(req, sink, "fleet-execution contract key is unenforceable: "+keyErr.Error())
		}
		gen := req.LeaseGeneration
		if gen < 1 && req.FleetBinding.LeaseGeneration > 0 {
			gen = req.FleetBinding.LeaseGeneration
		}
		if err := agentpolicy.RequireLaunchBinding(req.FleetBinding, key, gen); err != nil {
			return reject(req, sink, "fleet-execution contract is absent, stale, or mismatched: "+err.Error())
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

// BindingFromReceipt reconstructs the public fleet binding carried on a
// launch/recovery receipt. Empty fields yield a zero binding that
// VerifyFieldsPresent rejects.
func BindingFromReceipt(r Receipt) agentpolicy.LaunchBinding {
	return agentpolicy.LaunchBinding{
		Repository:            r.Repository,
		Task:                  r.TaskRef,
		Lane:                  r.Lane,
		Role:                  r.Role,
		LeaseGeneration:       r.LeaseGeneration,
		HerdrSession:          r.HerdrSession,
		HerdrTab:              r.TabID,
		HerdrPane:             r.PaneID,
		ParentExecutionFamily: r.FleetFamily,
		AllowedHerdrSurface:   r.FleetSurface,
		PolicyDigest:          r.FleetPolicyDigest,
		AuthTag:               r.FleetAuthTag,
	}
}

// VerifyRecoveryBinding fails closed when a recovery packet cannot re-prove
// the fleet-execution contract at the exact lease generation.
func VerifyRecoveryBinding(r Receipt, key []byte, generation int64) error {
	b := BindingFromReceipt(r)
	return agentpolicy.RequireLaunchBinding(b, key, generation)
}

// argvCarriesModel reports whether argv names a model explicitly. It looks for
// the flag anywhere with a non-flag value after it, rather than at a fixed
// index, so prepended vendor flags cannot make a well-formed argv look
// model-less.
func argvCarriesModel(argv []string) bool {
	for i, a := range argv {
		if a != "--model" {
			continue
		}
		if i+1 < len(argv) && strings.TrimSpace(argv[i+1]) != "" && !strings.HasPrefix(argv[i+1], "-") {
			return true
		}
	}
	return false
}

// PreflightHooks checks the configured hook boundary without creating tabs,
// processes, prompts, board records, or worktrees. It is also used by resume
// recovery so a previously healthy session cannot bypass current hook health.
func PreflightHooks(req Request) (harness.HookReport, error) {
	return preflightHooks(req, nil)
}

func preflightHooks(req Request, sink Sink) (harness.HookReport, error) {
	report := harness.HookReport{RequiredHealthy: true}
	if req.Decision == nil || strings.TrimSpace(req.Decision.Provider) == "" || strings.TrimSpace(req.Decision.Model) == "" || strings.TrimSpace(req.Decision.Effort) == "" {
		return report, fmt.Errorf("launch hook preflight failed: %s", harness.HookCodeMalformed)
	}
	discovery := req.HookDiscovery
	if discovery == nil {
		discovery = harness.DefaultDiscovery{}
	}
	result, err := discovery.Discover(req.Decision.Provider)
	if err != nil || result.State == harness.DiscoveryFailed {
		return report, recordHookFailure(req, sink, harness.HookCodeDiscoveryFailed, "", harness.EndpointInvalid, "")
	}
	if result.State == harness.DiscoveryNotDiscovered {
		return report, recordHookFailure(req, sink, harness.HookCodeDiscoveryMissing, "", harness.EndpointInvalid, "")
	}
	if result.State != harness.DiscoveryNoHooks && result.State != harness.DiscoveryHooks {
		return report, recordHookFailure(req, sink, harness.HookCodeDiscoveryFailed, "", harness.EndpointInvalid, "")
	}
	if result.State == harness.DiscoveryNoHooks && len(result.Hooks) != 0 {
		return report, recordHookFailure(req, sink, harness.HookCodeMalformed, "", harness.EndpointInvalid, "")
	}
	if result.State == harness.DiscoveryHooks && len(result.Hooks) == 0 {
		return report, recordHookFailure(req, sink, harness.HookCodeDiscoveryFailed, "", harness.EndpointInvalid, "")
	}
	req.HookPolicyRevision = result.PolicyRevision
	if result.PolicyRequired {
		bound, code, digest := harness.ApplyHookPolicies(result.Hooks, result.Policies, result.PolicyRevision)
		if code != harness.HookCodeHealthy {
			return report, recordHookFailure(req, sink, code, digest, harness.EndpointInvalid, "")
		}
		result.Hooks = bound
	}
	req.Hooks = result.Hooks
	identity := harness.HookIdentity{Provider: req.Decision.Provider, Model: req.Decision.Model, Effort: req.Decision.Effort, PolicyRevision: req.HookPolicyRevision}
	report = harness.CheckHooksWithOptions(context.Background(), result.Hooks, identity, req.HookClient, harness.HookCheckOptions{ApprovedAuthorities: result.ApprovedAuthorities})
	if report.DegradedWarning != "" {
		written, err := recordHookDegraded(req, sink, report)
		if err != nil {
			return report, err
		}
		if written && req.HookWarning != nil {
			req.HookWarning(report.DegradedWarning)
		}
	}
	if !report.RequiredHealthy {
		return report, recordFirstHookFailure(req, sink, report)
	}
	if report.DegradedWarning == "" {
		if err := recordHookRecovered(req, sink); err != nil {
			return report, err
		}
	}
	return report, nil
}

func recordFirstHookFailure(req Request, sink Sink, report harness.HookReport) error {
	for _, result := range report.Results {
		if result.Status != harness.HookHealthy && (result.Requirement == harness.HookRequired || harness.IsPolicyCode(result.Code)) {
			return recordHookFailure(req, sink, result.Code, result.Name, result.EndpointClass, result.RedactedAuthority)
		}
	}
	return fmt.Errorf("launch hook preflight failed: %s", harness.HookCodeMalformed)
}

func hookReceiptKey(req Request, code harness.HookCode, name string) string {
	return fmt.Sprintf("hook|%s|%d|%s|%s|%s|%s|%s", req.TaskRef, req.LeaseGeneration, req.HookPolicyRevision, DecisionDigest(req.Decision), name, string(code), req.Scope)
}

func recordHookFailure(req Request, sink Sink, code harness.HookCode, name string, endpoint harness.EndpointClass, authority string) error {
	receipt := Receipt{CreatedAt: time.Now().UTC(), Kind: "launch_rejected", TaskRef: req.TaskRef, LeaseGeneration: req.LeaseGeneration, PolicyRevision: req.HookPolicyRevision, DecisionDigest: DecisionDigest(req.Decision), HookCode: string(code), HookName: name, EndpointClass: string(endpoint), RedactedAuthority: authority, ReceiptKey: hookReceiptKey(req, code, name)}
	if _, err := writeOnce(sink, receipt); err != nil {
		return fmt.Errorf("launch hook preflight failed: %s", code)
	}
	return fmt.Errorf("launch hook preflight failed: %s", code)
}

func recordHookDegraded(req Request, sink Sink, report harness.HookReport) (bool, error) {
	names := make([]string, 0)
	authorities := make([]string, 0)
	classes := make([]string, 0)
	for _, result := range report.Results {
		if result.Requirement == harness.HookOptional && result.Status != harness.HookHealthy && !harness.IsPolicyCode(result.Code) {
			names = append(names, result.Name)
			authorities = append(authorities, result.RedactedAuthority)
			classes = append(classes, string(result.EndpointClass))
		}
	}
	if len(names) == 0 {
		return false, nil
	}
	receipt := Receipt{CreatedAt: time.Now().UTC(), Kind: "hook_degraded", TaskRef: req.TaskRef, LeaseGeneration: req.LeaseGeneration, PolicyRevision: req.HookPolicyRevision, DecisionDigest: DecisionDigest(req.Decision), HookCode: string(harness.HookCodeDegraded), HookName: strings.Join(names, ","), EndpointClass: strings.Join(classes, ","), RedactedAuthority: strings.Join(authorities, ","), ReceiptKey: hookReceiptKey(req, harness.HookCodeDegraded, strings.Join(names, ","))}
	written, err := writeOnce(sink, receipt)
	if err != nil {
		return false, fmt.Errorf("launch hook preflight failed: %s", harness.HookCodeDegraded)
	}
	return written, nil
}

func recordHookRecovered(req Request, sink Sink) error {
	if sink == nil {
		sink = DefaultSink()
	}
	history, ok := sink.(degradedHistory)
	if !ok {
		return nil
	}
	hasDegraded, err := history.HasDegraded(req)
	if err != nil || !hasDegraded {
		return err
	}
	receipt := Receipt{CreatedAt: time.Now().UTC(), Kind: "hook_recovered", TaskRef: req.TaskRef, LeaseGeneration: req.LeaseGeneration, PolicyRevision: req.HookPolicyRevision, DecisionDigest: DecisionDigest(req.Decision), HookCode: "hook.recovered", HookName: "all", EndpointClass: "local", ReceiptKey: hookReceiptKey(req, harness.HookCode("hook.recovered"), "all")}
	if _, err := writeOnce(sink, receipt); err != nil {
		return fmt.Errorf("launch hook preflight failed: hook.recovered")
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
