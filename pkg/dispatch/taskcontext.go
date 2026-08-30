package dispatch

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// TaskContextFile is the launch receipt written into every isolated agent
// worktree at spawn (FAC-145). Agents and provider adapters read it instead
// of depending on an ignored checkout-local context file or the operator's
// focused repository. It is the SOLE provider-context authority in a
// worktree; no provider-native context file is seeded (a .kaneo.json
// convenience file was an unguarded ambient-mutation affordance and a
// crash-consistency hazard).
const TaskContextFile = "TASK-CONTEXT.json"

// DefaultReceiptTTL bounds how long a launch receipt authorizes provider
// operations when the dispatch carries no explicit lease expiry. Cards run
// long; a day covers any legitimate build without leaving an immortal
// mutation authority in an abandoned worktree.
const DefaultReceiptTTL = 24 * time.Hour

// Receipt roles. The role is the callback SenderRole and scopes which
// operations a receipt may authorize.
const (
	RoleWorker      = "worker"
	RoleReviewer    = "reviewer"
	RoleVerifier    = "verifier"    // verification-gate agents
	RoleRecovery    = "recovery"    // recovery-sentinel agents
	RoleIntegration = "integration" // harvest/integration owners
	RoleCoordinator = "coordinator"
	// RoleScoutPlanner is a read-mostly lane role. It is deliberately scoped
	// to worker operations (get/list/comment) and can never authorize board
	// mutation, but must remain a first-class task-context role because the
	// configured scout-planner lane signs its own dispatch receipts.
	RoleScoutPlanner = "scout-planner"
)

// WorkerOps / ReviewerOps / CoordinatorOps are the default allowed-operation
// sets per role. Workers and reviewers read and comment; only coordinator
// contexts may mutate board state (claim/status transitions) — the fleet
// protocol keeps card moves coordinator-owned.
var (
	WorkerOps   = []string{string(provider.OpGet), string(provider.OpList), string(provider.OpComment)}
	ReviewerOps = []string{string(provider.OpGet), string(provider.OpList), string(provider.OpComment)}
	// Verification gates and recovery sentinels observe and annotate; they
	// never move cards.
	VerifierOps = []string{string(provider.OpGet), string(provider.OpList), string(provider.OpComment)}
	RecoveryOps = []string{string(provider.OpGet), string(provider.OpList), string(provider.OpComment)}
	// Integration owners read broadly to reconcile merges but still never
	// mutate: board transitions stay coordinator-owned.
	IntegrationOps = []string{string(provider.OpGet), string(provider.OpList), string(provider.OpComment)}
	CoordinatorOps = []string{string(provider.OpGet), string(provider.OpList), string(provider.OpComment), string(provider.OpMutate)}
)

// OpsForRole returns the sanctioned operation set for a known role, or nil
// for an unknown role (which then fails Validate).
func OpsForRole(role string) []string {
	switch role {
	case RoleWorker:
		return WorkerOps
	case RoleReviewer:
		return ReviewerOps
	case RoleVerifier:
		return VerifierOps
	case RoleRecovery:
		return RecoveryOps
	case RoleIntegration:
		return IntegrationOps
	case RoleScoutPlanner:
		return WorkerOps
	case RoleCoordinator:
		return CoordinatorOps
	default:
		return nil
	}
}

// TaskContext binds an isolated agent to its repository, task provider,
// project, task ref, candidate/base commits, lease generation, role,
// allowed operations, and expiry. It never carries credentials or absolute
// host paths — only references (FAC-145). ProviderProfile is the NAME of
// the credential source (e.g. the configured api_key_env), never the secret
// itself. Signature is coordinator-issued (ed25519, private key outside the
// repo tree); see authority.go.
type TaskContext struct {
	ProviderType      string `json:"provider_type"`
	ProjectID         string `json:"project_id"`
	ProviderWorkspace string `json:"provider_workspace,omitempty"` // e.g. kaneo workspace ID
	ProviderProfile   string `json:"provider_profile,omitempty"`   // credential profile reference, never a secret
	Repository        string `json:"repository"`                   // repo identity from config, never a path
	Role              string `json:"role"`
	TaskRef           string `json:"task_ref"`
	TaskID            string `json:"task_id"`
	Branch            string `json:"branch"`
	BaseSHA           string `json:"base_sha"`
	CandidateSHA      string `json:"candidate_sha,omitempty"` // exact commit under review (review-issued receipts)
	AnchorRef         string `json:"anchor_ref,omitempty"`
	HerdrWorkspace    string `json:"herdr_workspace,omitempty"` // set once launch resolves it
	LeaseID           string `json:"lease_id"`
	LeaseGeneration   int64  `json:"lease_generation"`
	// LeaseTaskRef is the EXACT task ref the durable claim lease was
	// acquired under. Reviewer leases are scoped (e.g. "FAC-9:review") so a
	// review never collides with the builder's claim; consumers must fence
	// against THIS key, never a reconstructed one (FAC-145).
	LeaseTaskRef string `json:"lease_task_ref"`
	// SessionID identifies THIS isolated agent session (role + candidate +
	// launch). Durable canonical copies are keyed by it, so concurrent
	// roles and candidates on one task never collide (FAC-145).
	SessionID string `json:"session_id"`
	// AgentSessionID is the launcher's session/tab binding once resolved.
	AgentSessionID string    `json:"agent_session_id,omitempty"`
	AllowedOps     []string  `json:"allowed_ops"`
	ExpiresAt      time.Time `json:"expires_at"`
	Signature      string    `json:"signature,omitempty"`
}

// Validate fails closed on any context that could produce a NULL-project
// provider read, an unattributable mutation, an unbounded or unfenced
// authority, or a host-path leak downstream. LeaseID/LeaseGeneration are
// REQUIRED and come from the durable claim store's acquired lease — no
// generation is ever fabricated; FAC-147's canonical fence consumes the
// same fields at the rebase.
func (tc TaskContext) Validate() error {
	for _, f := range []struct{ name, val string }{
		{"provider_type", tc.ProviderType},
		{"project_id", tc.ProjectID},
		{"repository", tc.Repository},
		{"role", tc.Role},
		{"task_ref", tc.TaskRef},
		{"task_id", tc.TaskID},
		{"branch", tc.Branch},
		{"base_sha", tc.BaseSHA},
		{"lease_id", tc.LeaseID},
		{"lease_task_ref", tc.LeaseTaskRef},
		{"session_id", tc.SessionID},
	} {
		if strings.TrimSpace(f.val) == "" {
			return fmt.Errorf("task context %s is required (FAC-145 fail-closed; refusing a context that would resolve NULL %s)", f.name, f.name)
		}
	}
	if tc.LeaseGeneration < 1 {
		return fmt.Errorf("task context lease_generation %d is invalid (FAC-145: unfenced receipts carry no authority)", tc.LeaseGeneration)
	}
	switch tc.Role {
	case RoleWorker, RoleCoordinator, RoleVerifier, RoleRecovery, RoleIntegration, RoleScoutPlanner:
	case RoleReviewer:
		// Exact-candidate review: a reviewer receipt without the precise
		// commit under review can verdict nothing.
		if strings.TrimSpace(tc.CandidateSHA) == "" {
			return fmt.Errorf("task context reviewer receipt requires candidate_sha (FAC-145 fail-closed; reviews bind to the exact candidate)")
		}
	default:
		return fmt.Errorf("task context role %q is unknown — unknown roles carry no authority (FAC-145 fail-closed)", tc.Role)
	}
	if len(tc.AllowedOps) == 0 {
		return fmt.Errorf("task context allowed_ops is required (FAC-145 fail-closed; a receipt with no operations authorizes nothing and must say so explicitly)")
	}
	for _, op := range tc.AllowedOps {
		switch provider.OpKind(op) {
		case provider.OpGet, provider.OpList, provider.OpMutate, provider.OpComment:
		default:
			return fmt.Errorf("task context allowed op %q is unknown (FAC-145 fail-closed)", op)
		}
		// Role-to-operation policy: only the coordinator may ever carry
		// mutate. An over-privileged agent receipt is invalid at the root,
		// not merely unauthorized per-call.
		if op == string(provider.OpMutate) && tc.Role != RoleCoordinator {
			return fmt.Errorf("task context role %q must not carry op %q (FAC-145: board mutations are coordinator-owned)", tc.Role, op)
		}
	}
	if tc.ExpiresAt.IsZero() {
		return fmt.Errorf("task context expires_at is required (FAC-145 fail-closed; an immortal receipt is an unbounded mutation authority)")
	}
	// No absolute host paths anywhere in the receipt: it must stay portable
	// across machines and never leak operator filesystem layout.
	for _, f := range []struct{ name, val string }{
		{"provider_type", tc.ProviderType}, {"project_id", tc.ProjectID},
		{"provider_workspace", tc.ProviderWorkspace}, {"provider_profile", tc.ProviderProfile},
		{"repository", tc.Repository}, {"role", tc.Role},
		{"task_ref", tc.TaskRef}, {"task_id", tc.TaskID},
		{"branch", tc.Branch}, {"base_sha", tc.BaseSHA},
		{"candidate_sha", tc.CandidateSHA},
		{"anchor_ref", tc.AnchorRef}, {"herdr_workspace", tc.HerdrWorkspace},
		{"lease_id", tc.LeaseID}, {"lease_task_ref", tc.LeaseTaskRef},
		{"session_id", tc.SessionID}, {"agent_session_id", tc.AgentSessionID},
	} {
		if filepath.IsAbs(f.val) {
			return fmt.Errorf("task context %s %q is an absolute host path (FAC-145: receipts carry references, never paths)", f.name, f.val)
		}
	}
	return nil
}

// NewSessionID derives the durable identity of ONE isolated agent session
// from its role, task, candidate, and launch nonce.
func NewSessionID(role, taskRef, candidateSHA, nonce string) string {
	sum := sha256.Sum256([]byte(role + "\x00" + strings.ToLower(taskRef) + "\x00" + candidateSHA + "\x00" + nonce))
	return fmt.Sprintf("%s-%s", role, hex.EncodeToString(sum[:8]))
}

// Authorize reports whether the receipt permits op at now. Expired receipts
// and operations outside AllowedOps are rejected — before any provider call.
func (tc TaskContext) Authorize(now time.Time, op provider.OpKind) error {
	if now.After(tc.ExpiresAt) {
		return fmt.Errorf("task context for %s expired at %s (FAC-145: expired receipts authorize nothing)", tc.TaskRef, tc.ExpiresAt.Format(time.RFC3339))
	}
	for _, a := range tc.AllowedOps {
		if a == string(op) {
			return nil
		}
	}
	return fmt.Errorf("task context for %s (role %s) does not allow op %q (FAC-145: over-privileged use rejected)", tc.TaskRef, tc.Role, op)
}

// ForRepository rejects a receipt raised under a different repository — a
// differently focused repo/workspace can never redirect a provider call.
func (tc TaskContext) ForRepository(repo string) error {
	if !strings.EqualFold(strings.TrimSpace(repo), tc.Repository) {
		return fmt.Errorf("task context for %s is bound to repository %q, not %q (FAC-145: cross-repository use rejected)", tc.TaskRef, tc.Repository, repo)
	}
	return nil
}

// BoundCallback derives the agent callback for this receipt. Worker FAIL and
// coordinator PASS callbacks both come through here, so both carry the
// identical repo + lease-generation + role binding (FAC-145).
func (tc TaskContext) BoundCallback(kind mail.CallbackKind, sha, detail string) (mail.Callback, error) {
	if err := tc.Validate(); err != nil {
		return mail.Callback{}, err
	}
	if kind == mail.CallbackComplete && strings.TrimSpace(sha) == "" {
		return mail.Callback{}, fmt.Errorf("completion callback for %s requires a SHA (FAC-145: verdicts are SHA-bound)", tc.TaskRef)
	}
	return mail.Callback{
		Ref:             tc.TaskRef,
		Kind:            kind,
		SHA:             sha,
		Detail:          detail,
		Repo:            tc.Repository,
		LeaseGeneration: tc.LeaseGeneration,
		SenderRole:      tc.Role,
	}, nil
}

// CanonicalTaskContextDir is the coordinator's DURABLE task-context store,
// relative to the repository root. Worktrees are ephemeral (GC/reap); the
// canonical copy of every issued authorization lives here so authorization
// consumers can still bind after the worktree is gone. Coordinator-written,
// 0600, signature-verified by authority-bearing consumers — this is issued
// dispatch authority, not completion evidence or a config-derived fallback.
//
// Completion receipts deliberately remain in .herd/receipts/<REF>.json. The
// schemas must not share a directory: a task context proves authorization to
// work, while a completion receipt proves that reviewed work landed.
const CanonicalTaskContextDir = ".herd/task-context-receipts"

// safeRefComponent rejects any task ref that cannot be used as a single
// path component: untrusted provider refs must never traverse out of the
// receipt store (FAC-145).
var refComponentPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

func safeRefComponent(ref string) error {
	if !refComponentPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return fmt.Errorf("task ref %q is not a safe path component (FAC-145: refusing traversal-capable ref)", ref)
	}
	return nil
}

// StoreCanonicalReceipt durably persists the issued receipt outside the
// ephemeral worktree (unique temp + fsync + atomic rename + dir fsync).
// Storage is SERIALIZED under a per-store flock and MONOTONIC: an existing
// canonical receipt with a newer lease generation can never be overwritten
// by an older writer (rollback refused); the committed file is re-read and
// must round-trip exactly (mandatory readback).
func StoreCanonicalReceipt(root string, tc TaskContext) error {
	if err := tc.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(tc.Signature) == "" {
		return fmt.Errorf("refusing to store an unsigned receipt as canonical authority (FAC-145)")
	}
	if err := safeRefComponent(tc.TaskRef); err != nil {
		return err
	}
	dir := filepath.Join(root, CanonicalTaskContextDir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create canonical receipt dir: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, ".lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return fmt.Errorf("open receipt store lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock receipt store: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	if existing, exErr := LoadCanonicalReceiptSession(root, tc.ProviderType, tc.ProjectID, tc.TaskRef, tc.SessionID); exErr == nil {
		if existing.LeaseGeneration > tc.LeaseGeneration {
			return fmt.Errorf("canonical receipt for %s holds generation %d; refusing rollback to generation %d (FAC-145 monotonic authority)",
				tc.TaskRef, existing.LeaseGeneration, tc.LeaseGeneration)
		}
		if existing.LeaseGeneration == tc.LeaseGeneration {
			// Same-generation transitions are IMMUTABLE-field-preserving:
			// the only sanctioned change is stamping the resolved herdr
			// workspace (empty -> value, or unchanged). Any other field
			// difference is a delayed old writer or a forgery attempt.
			if err := existing.validSameGenerationTransition(tc); err != nil {
				return err
			}
		}
	} else if !errors.Is(exErr, os.ErrNotExist) {
		return fmt.Errorf("cannot audit existing canonical receipt (FAC-145 fail-closed): %w", exErr)
	}
	data, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal canonical receipt: %w", err)
	}
	path := filepath.Join(dir, canonicalReceiptName(tc.ProviderType, tc.ProjectID, tc.TaskRef, tc.SessionID))
	tmp, err := os.CreateTemp(dir, "receipt.tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	fail := func(step string, cause error) error {
		tmp.Close()
		os.Remove(name)
		return fmt.Errorf("%s: %w", step, cause)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail("write canonical receipt", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync canonical receipt", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close canonical receipt", err)
	}
	if err := os.Chmod(name, 0600); err != nil {
		return fail("chmod canonical receipt", err)
	}
	if err := os.Rename(name, path); err != nil {
		return fail("commit canonical receipt", err)
	}
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open canonical receipt dir for sync: %w", err)
	}
	if err := d.Sync(); err != nil {
		d.Close()
		return fmt.Errorf("sync canonical receipt dir: %w", err)
	}
	if err := d.Close(); err != nil {
		return err
	}
	// Mandatory readback: the committed canonical receipt must round-trip
	// exactly — a torn or diverted write is surfaced here, not discovered at
	// recovery time.
	back, err := LoadCanonicalReceiptSession(root, tc.ProviderType, tc.ProjectID, tc.TaskRef, tc.SessionID)
	if err != nil {
		return fmt.Errorf("canonical receipt readback failed (FAC-145): %w", err)
	}
	if !back.EqualsIssued(tc) {
		return fmt.Errorf("canonical receipt readback mismatch for %s (FAC-145)", tc.TaskRef)
	}
	return nil
}

// validSameGenerationTransition permits exactly one same-generation change:
// the herdr workspace stamp (empty -> value or unchanged). Every other
// field — candidate, role, ops, lease id, expiry, identity — is immutable
// at a given generation; a delayed older signed receipt can never replace
// them (FAC-145).
func (existing TaskContext) validSameGenerationTransition(next TaskContext) error {
	if existing.HerdrWorkspace != "" && next.HerdrWorkspace != existing.HerdrWorkspace {
		return fmt.Errorf("canonical receipt for %s: herdr workspace is already stamped %q and cannot change to %q at the same generation (FAC-145)",
			existing.TaskRef, existing.HerdrWorkspace, next.HerdrWorkspace)
	}
	frozenExisting := existing
	frozenNext := next
	frozenExisting.HerdrWorkspace, frozenNext.HerdrWorkspace = "", ""
	frozenExisting.Signature, frozenNext.Signature = "", ""
	a, errA := json.Marshal(frozenExisting)
	b, errB := json.Marshal(frozenNext)
	if errA != nil || errB != nil {
		return fmt.Errorf("canonical receipt comparison failed (FAC-145)")
	}
	if string(a) != string(b) {
		return fmt.Errorf("canonical receipt for %s: same-generation write changes immutable fields — refused (FAC-145)", existing.TaskRef)
	}
	return nil
}

// EqualsIssued compares every issued field including the signature.
func (tc TaskContext) EqualsIssued(other TaskContext) bool {
	a, errA := json.Marshal(tc)
	b, errB := json.Marshal(other)
	return errA == nil && errB == nil && string(a) == string(b)
}

// LoadCanonicalReceipt loads + structurally validates the durable receipt
// for ref. Consumers must still authenticate the signature.
// canonicalReceiptName keys a durable receipt by provider/project/task and
// SESSION, so concurrent roles or candidates for one task never collide.
func canonicalReceiptName(providerType, projectID, taskRef, sessionID string) string {
	sum := sha256.Sum256([]byte(providerType + "\x00" + projectID + "\x00" + strings.ToLower(taskRef) + "\x00" + sessionID))
	return fmt.Sprintf("%s-%s.json", strings.ToLower(taskRef), hex.EncodeToString(sum[:8]))
}

// LoadCanonicalReceiptSession loads the durable copy for one exact session.
func LoadCanonicalReceiptSession(root, providerType, projectID, ref, sessionID string) (TaskContext, error) {
	var tc TaskContext
	if err := safeRefComponent(ref); err != nil {
		return tc, err
	}
	return readCanonicalFile(filepath.Join(root, CanonicalTaskContextDir,
		canonicalReceiptName(providerType, projectID, ref, sessionID)), ref)
}

// LoadCanonicalReceipt returns the newest durable receipt for ref across
// sessions — the recovery entry when only the task ref is known.
func LoadCanonicalReceipt(root, ref string) (TaskContext, error) {
	var tc TaskContext
	if err := safeRefComponent(ref); err != nil {
		return tc, err
	}
	dir := filepath.Join(root, CanonicalTaskContextDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return tc, fmt.Errorf("no canonical receipt for %s (FAC-145): %w", ref, err)
	}
	prefix := strings.ToLower(ref) + "-"
	var best TaskContext
	found := false
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), prefix) || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		cand, cErr := readCanonicalFile(filepath.Join(dir, e.Name()), ref)
		if cErr != nil {
			return tc, cErr
		}
		if !found || cand.LeaseGeneration > best.LeaseGeneration {
			best, found = cand, true
		}
	}
	if !found {
		return tc, fmt.Errorf("no canonical receipt for %s (FAC-145): %w", ref, os.ErrNotExist)
	}
	return best, nil
}

func readCanonicalFile(path, ref string) (TaskContext, error) {
	var tc TaskContext
	data, err := os.ReadFile(path)
	if err != nil {
		return tc, fmt.Errorf("no canonical receipt for %s (FAC-145): %w", ref, err)
	}
	if err := json.Unmarshal(data, &tc); err != nil {
		return tc, fmt.Errorf("corrupt canonical receipt for %s: %w", ref, err)
	}
	if err := tc.Validate(); err != nil {
		return tc, err
	}
	return tc, nil
}

// ReadTaskContext loads and structurally validates the launch receipt from
// a worktree. A missing or invalid receipt is a hard error: enforcement
// consumers must fail closed rather than fall back to ambient repository
// state. NOTE: this does NOT authenticate the receipt — authority-bearing
// consumers must additionally Verify it against the published key.
func ReadTaskContext(worktreePath string) (TaskContext, error) {
	var tc TaskContext
	data, err := os.ReadFile(filepath.Join(worktreePath, TaskContextFile))
	if err != nil {
		return tc, fmt.Errorf("no usable task context in %s (FAC-145 fail-closed; refusing ambient fallback): %w", worktreePath, err)
	}
	if err := json.Unmarshal(data, &tc); err != nil {
		return tc, fmt.Errorf("corrupt task context in %s: %w", worktreePath, err)
	}
	if err := tc.Validate(); err != nil {
		return tc, err
	}
	return tc, nil
}

// WriteTaskContext durably publishes the launch receipt into the worktree
// root: staged to a UNIQUE temp file (no fixed-name races), fsynced, then
// atomically renamed into place, then the directory is fsynced — a crash at
// any point leaves either the old receipt or the new one, never a torn or
// missing authority. Only signed receipts should be persisted; issuers go
// through Signer.Issue first. Idempotent: re-writing with an updated
// context (e.g. after herdr workspace resolution) is expected.
func WriteTaskContext(worktreePath string, tc TaskContext) error {
	if err := tc.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(tc, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal task context: %w", err)
	}
	path := filepath.Join(worktreePath, TaskContextFile)
	tmp, err := os.CreateTemp(worktreePath, TaskContextFile+".tmp-*")
	if err != nil {
		return fmt.Errorf("stage task context: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(step string, cause error) error {
		tmp.Close()
		if rmErr := os.Remove(tmpName); rmErr != nil && !os.IsNotExist(rmErr) {
			return fmt.Errorf("%s: %w (cleanup: %v)", step, cause, rmErr)
		}
		return fmt.Errorf("%s: %w", step, cause)
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		return fail("write task context", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync task context", err)
	}
	if err := tmp.Close(); err != nil {
		return fail("close task context", err)
	}
	if err := os.Chmod(tmpName, 0644); err != nil {
		return fail("chmod task context", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fail("commit task context", err)
	}
	// Directory durability failures fail CLOSED — a receipt that may vanish
	// on crash is not an issued authority.
	dir, err := os.Open(worktreePath)
	if err != nil {
		return fmt.Errorf("open worktree dir for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("sync worktree dir: %w", err)
	}
	return dir.Close()
}
