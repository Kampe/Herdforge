package security

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// EnforcedControlState is sealed, MAC-reverified control for worker runtime.
// Verified is never trusted alone — LoadSealedControl always re-checks MAC.
type EnforcedControlState struct {
	// Control is the full signed envelope (required for re-verification).
	Control *envelope.Envelope `json:"control"`
	// Binding fields duplicated for cheap inspection after re-verify.
	EnvelopeID      string `json:"envelope_id"`
	Sequence        uint64 `json:"sequence"`
	Task            string `json:"task"`
	WorkerSession   string `json:"worker_session"`
	LeaseGeneration int64  `json:"lease_generation"`
}

// SealedControlPath is outside the agent worktree write root (under shared
// checkout .herd). Seatbelt denies agent write to shared, so the worker cannot
// rewrite sealed control after launch.
func SealedControlPath(sharedRoot, task, workerSession string) string {
	return filepath.Join(sharedRoot, ".herd", "control", "sealed",
		sanitizeName(task)+"__"+sanitizeName(workerSession)+".json")
}

// SealedControlBarrierPath is the start-barrier seal keyed by task+lease+version
// so retries cannot poison/replay a stale barrier file. version must be a
// fresh nonce per launch attempt. The seal body carries the pre-start worker
// identity (herdr-pane:… or live session) — never pending-*.
func SealedControlBarrierPath(sharedRoot, task string, lease int64, version string) string {
	if strings.TrimSpace(version) == "" {
		version = "0"
	}
	return filepath.Join(sharedRoot, ".herd", "control", "sealed",
		fmt.Sprintf("%s__g%d__v%s.barrier.json", sanitizeName(task), lease, sanitizeName(version)))
}

// ClearStaleBarriers removes prior task+lease barrier seals so a retry cannot
// verify against a poisoned/stale file from a previous attempt.
func ClearStaleBarriers(sharedRoot, task string, lease int64) error {
	dir := filepath.Join(sharedRoot, ".herd", "control", "sealed")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	prefix := fmt.Sprintf("%s__g%d__", sanitizeName(task), lease)
	var first error
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".barrier.json") {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && first == nil {
				first = err
			}
		}
		// Legacy unversioned path from earlier FAC-133 drafts.
		legacy := fmt.Sprintf("%s__g%d.barrier.json", sanitizeName(task), lease)
		if name == legacy {
			if err := os.Remove(filepath.Join(dir, name)); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

// PreStartWorkerSession is DEPRECATED for production control binding.
// Pane identity is not an AgentSessionID; production must bind live session
// after start and refuse pane/term placeholders via RefuseProvisionalWorkerSession.
func PreStartWorkerSession(paneID string) string {
	paneID = strings.TrimSpace(paneID)
	if paneID == "" {
		return ""
	}
	return "herdr-pane:" + paneID
}

// RefuseProvisionalWorkerSession fails closed on pending/pane/term/probe/spawn ids.
// Production control seals must use a live agent_session or a live-confirmed
// live-agent:name|tab|pane composite — never a fabricated ses_spawn_* seed.
func RefuseProvisionalWorkerSession(worker string) error {
	worker = strings.TrimSpace(worker)
	if worker == "" {
		return fmt.Errorf("%w: empty worker session", envelope.ErrMissingBinding)
	}
	bad := []string{
		"pending-", "herdr-pane:", "herdr-term:",
		"ses_probe_", "ses_real_", "ses_spawn_", "test-session-",
	}
	for _, p := range bad {
		if strings.HasPrefix(worker, p) {
			return fmt.Errorf("%w: provisional/placeholder worker %q refused (need live AgentSessionID or live-agent binding)", envelope.ErrMissingBinding, worker)
		}
	}
	return nil
}

// LiveWorkerBinding returns the control-plane worker identity for a live agent.
// Prefer a real agent_session when present and non-provisional; otherwise a
// live-confirmed name|tab|pane composite (session-less kinds such as grok).
func LiveWorkerBinding(name, tabID, paneID, agentSession string) (string, error) {
	if sid := strings.TrimSpace(agentSession); sid != "" {
		if err := RefuseProvisionalWorkerSession(sid); err != nil {
			return "", err
		}
		return sid, nil
	}
	name = strings.TrimSpace(name)
	tabID = strings.TrimSpace(tabID)
	paneID = strings.TrimSpace(paneID)
	if name == "" || tabID == "" || paneID == "" {
		return "", fmt.Errorf("%w: session-less live bind requires name+tab+pane", envelope.ErrMissingBinding)
	}
	// live-agent: is not in the provisional denylist; it is only minted after
	// a live census confirmed the triple.
	return "live-agent:" + name + "|" + tabID + "|" + paneID, nil
}

// EnforcedControlPath is worktree-local mirror (optional); Load prefers sealed.
func EnforcedControlPath(worktree string) string {
	return filepath.Join(worktree, ".herd", "control", "enforced.json")
}

func sanitizeName(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			out = append(out, r)
		} else {
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "x"
	}
	return string(out)
}

// ApplyControlScopeToPolicy mutates policy/grant from a verified scope BEFORE
// containment Install so the seatbelt profile encodes exclusive packages.
// Exclusive with empty PackageAllowlist is refused (would be a no-op mutation).
func ApplyControlScopeToPolicy(policy *LaunchPolicy, grant *LaunchGrant, scope *envelope.Scope) error {
	if policy == nil {
		return fmt.Errorf("%w: nil policy", ErrUnknownPolicy)
	}
	if scope == nil {
		return fmt.Errorf("%w: nil scope", ErrUnknownPolicy)
	}
	if scope.Exclusive && len(scope.PackageAllowlist) == 0 {
		return fmt.Errorf("%w: exclusive scope requires non-empty PackageAllowlist (causal FS enforcement)", ErrUnknownPolicy)
	}
	if len(scope.PackageAllowlist) > 0 {
		norm, err := NormalizePackageAllowlist(scope.PackageAllowlist)
		if err != nil {
			return err
		}
		policy.PackageAllowlist = norm
		policy.ExclusivePackages = true
		if grant != nil {
			grant.PackageRoots = append([]string(nil), norm...)
		}
	} else if scope.Exclusive {
		return fmt.Errorf("%w: exclusive scope requires non-empty validated PackageAllowlist", ErrUnknownPolicy)
	}
	return nil
}

// ReinstallContainmentProfile rewrites the seatbelt profile + wrapper after a
// post-Install scope change so the running kernel boundary matches exclusive
// packages. Call after VerifyAndEnforceControl when the agent process may still
// re-exec the wrapper (or for standing agents that re-enter via wrapper).
func ReinstallContainmentProfile(policy *LaunchPolicy, grant *LaunchGrant, kind string) error {
	if policy == nil || grant == nil {
		return fmt.Errorf("%w: policy/grant required for reinstall", ErrUnknownPolicy)
	}
	backend, err := RequireContainment()
	if err != nil {
		return err
	}
	realBin, rerr := ResolveAgentBinary(kind)
	if rerr != nil {
		// Kind may be empty for control-only reinstall; try common worker path.
		if kind == "" {
			return fmt.Errorf("reinstall: kind required: %w", rerr)
		}
		return rerr
	}
	_, _, envFile, ierr := backend.Install(grant.CWD, policy, grant, kind, realBin)
	if ierr != nil {
		return fmt.Errorf("reinstall containment: %w", ierr)
	}
	// Re-publish sealed control bindings into env if present in ambient.
	_ = envFile
	_ = policy.record(EventKind("containment"), "profile_reinstalled", backend.Name())
	return nil
}

// VerifyAndEnforceControl MAC-verifies a signed control envelope, applies
// scope to policy/grant (for pre-Install containment), and seals the full
// signed envelope under sharedRoot (worker cannot rewrite).
//
// Must run BEFORE LaunchAgent Install so the profile encodes the allowlist.
// LoadSealedControl re-verifies MAC — Verified flags alone are never trusted.
func VerifyAndEnforceControl(secret string, ctrl *envelope.Envelope, policy *LaunchPolicy, grant *LaunchGrant, worktree, sharedRoot string) (*EnforcedControlState, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, envelope.ErrMissingSecret
	}
	if ctrl == nil {
		return nil, envelope.ErrNotControl
	}
	if err := ctrl.ValidateUnsigned(); err != nil {
		return nil, err
	}
	if !envelope.VerifyMAC([]byte(secret), ctrl) {
		return nil, envelope.ErrInvalidSignature
	}
	if policy == nil {
		return nil, fmt.Errorf("%w: policy required for enforce", ErrUnknownPolicy)
	}
	if err := ApplyControlScopeToPolicy(policy, grant, ctrl.Scope); err != nil {
		return nil, err
	}
	st := &EnforcedControlState{
		Control:         ctrl,
		EnvelopeID:      ctrl.ID,
		Sequence:        ctrl.Sequence,
		Task:            ctrl.TargetTask,
		WorkerSession:   ctrl.TargetWorkerSession,
		LeaseGeneration: ctrl.LeaseGeneration,
	}
	if sharedRoot != "" {
		if err := WriteSealedControl(sharedRoot, st); err != nil {
			return nil, err
		}
		// Versioned start-barrier path uses envelope id as version so retries
		// never share a path with a stale seal. Callers that already sealed
		// via PreStartSeal use the same path when ambient HERD_SEALED_CONTROL matches.
		if st.LeaseGeneration > 0 && st.Task != "" && st.EnvelopeID != "" {
			barrier := SealedControlBarrierPath(sharedRoot, st.Task, st.LeaseGeneration, st.EnvelopeID)
			if err := WriteSealedControlTo(barrier, st); err != nil {
				return nil, err
			}
		}
		// Coordinator-only MAC secret material for wrapper re-verify (outside
		// worktree; seatbelt denies agent write to shared).
		if err := WriteControlMACSecret(sharedRoot, secret); err != nil {
			return nil, err
		}
	}
	// Optional worktree mirror for tooling (still requires MAC re-verify on load).
	if worktree != "" {
		_ = WriteSealedControlTo(EnforcedControlPath(worktree), st)
	}
	_ = policy.record(EventKind("control"), "scope_enforced_pre_install", ctrl.ID)
	return st, nil
}

// WriteSealedControl persists sealed control under shared root.
func WriteSealedControl(sharedRoot string, st *EnforcedControlState) error {
	if sharedRoot == "" || st == nil || st.Control == nil {
		return fmt.Errorf("%w: sealed control requires shared root and signed envelope", ErrUnknownPolicy)
	}
	path := SealedControlPath(sharedRoot, st.Task, st.WorkerSession)
	return WriteSealedControlTo(path, st)
}

// WriteSealedControlTo writes sealed state with cross-process flock, unique
// tmp (no fixed .tmp race), O_NOFOLLOW-safe rewrite, and file+dir fsync.
func WriteSealedControlTo(path string, st *EnforcedControlState) error {
	if path == "" || st == nil || st.Control == nil {
		return fmt.Errorf("%w: sealed path/state", ErrUnknownPolicy)
	}
	// Refuse provisional sessions in sealed material.
	if strings.HasPrefix(st.Control.TargetWorkerSession, "pending-") {
		return fmt.Errorf("%w: provisional worker session cannot be sealed", envelope.ErrMissingBinding)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sealed control flock timeout: %s", lockPath)
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	// O_EXCL|O_NOFOLLOW where possible: create new file only.
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	// Replace: refuse if path is a symlink (no-follow intent).
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("sealed control path is symlink (refused)")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if serr := d.Sync(); serr != nil {
			_ = d.Close()
			return serr
		}
		_ = d.Close()
	}
	return nil
}

// LoadSealedControl loads and re-verifies MAC (never trusts a Verified flag).
// Binding must match expected task/worker/lease when provided (nonzero/non-empty).
func LoadSealedControl(path, secret string, expectTask, expectWorker string, expectLease int64) (*EnforcedControlState, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, envelope.ErrMissingSecret
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var st EnforcedControlState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("sealed control corrupt: %w", err)
	}
	if st.Control == nil {
		return nil, fmt.Errorf("%w: sealed control missing envelope", envelope.ErrNotControl)
	}
	if err := st.Control.ValidateUnsigned(); err != nil {
		return nil, err
	}
	if !envelope.VerifyMAC([]byte(secret), st.Control) {
		return nil, envelope.ErrInvalidSignature
	}
	// Binding checks (exact task/lease/session).
	if expectTask != "" && st.Control.TargetTask != expectTask {
		return nil, fmt.Errorf("%w: sealed task mismatch", envelope.ErrTaskMismatch)
	}
	if expectWorker != "" && st.Control.TargetWorkerSession != expectWorker {
		return nil, fmt.Errorf("%w: sealed worker mismatch", envelope.ErrWorkerMismatch)
	}
	if expectLease > 0 && st.Control.LeaseGeneration != expectLease {
		return nil, fmt.Errorf("%w: sealed lease mismatch", envelope.ErrStaleGeneration)
	}
	st.EnvelopeID = st.Control.ID
	st.Sequence = st.Control.Sequence
	st.Task = st.Control.TargetTask
	st.WorkerSession = st.Control.TargetWorkerSession
	st.LeaseGeneration = st.Control.LeaseGeneration
	return &st, nil
}

// LoadEnforcedControl is deprecated for trust decisions — always re-verify.
// Kept for callers that pass secret via LoadSealedControl on the same path.
func LoadEnforcedControl(worktree, secret string) (*EnforcedControlState, error) {
	if secret == "" {
		return nil, fmt.Errorf("%w: secret required to load enforced control (no Verified:true trust)", envelope.ErrMissingSecret)
	}
	return LoadSealedControl(EnforcedControlPath(worktree), secret, "", "", 0)
}

// FormatWorkerControlPayload is the agent-visible artifact: raw MAC envelope JSON.
func FormatWorkerControlPayload(ctrl *envelope.Envelope) (string, error) {
	if ctrl == nil {
		return "", envelope.ErrNotControl
	}
	raw, err := json.Marshal(ctrl)
	if err != nil {
		return "", err
	}
	return "HERD_CONTROL_ENVELOPE_JSON_V1 " + string(raw) + "\n", nil
}

// ControlMACSecretPath is the coordinator-only secret used by wrapper re-verify.
func ControlMACSecretPath(sharedRoot string) string {
	return filepath.Join(sharedRoot, ".herd", "control", "mac.secret")
}

// WriteControlMACSecret stores the MAC secret under shared root with flock,
// unique tmp, O_EXCL, no-follow replace, file+dir fsync, and readback.
func WriteControlMACSecret(sharedRoot, secret string) error {
	if sharedRoot == "" || secret == "" {
		return fmt.Errorf("%w: shared root and secret required", ErrUnknownPolicy)
	}
	path := ControlMACSecretPath(sharedRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	lockPath := path + ".lock"
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("mac.secret flock timeout")
		}
		time.Sleep(5 * time.Millisecond)
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()

	tmp := fmt.Sprintf("%s.%d.tmp", path, time.Now().UnixNano())
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write([]byte(secret)); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if fi, err := os.Lstat(path); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		_ = os.Remove(tmp)
		return fmt.Errorf("mac.secret path is symlink (refused)")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if d, err := os.Open(filepath.Dir(path)); err == nil {
		if serr := d.Sync(); serr != nil {
			_ = d.Close()
			return serr
		}
		_ = d.Close()
	}
	// Readback exact secret.
	got, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("mac.secret readback: %w", err)
	}
	if string(got) != secret {
		return fmt.Errorf("mac.secret readback mismatch")
	}
	return nil
}

// ReadControlMACSecret loads the coordinator MAC secret from shared root.
func ReadControlMACSecret(sharedRoot string) (string, error) {
	b, err := os.ReadFile(ControlMACSecretPath(sharedRoot))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// WorkerVerifySealed is the trusted worker-side re-verification hook invoked
// by the containment wrapper (before sandbox-exec) or by herd control verify.
// Exact task/worker/lease must match; provisional pending-* workers are refused.
// Empty expected task, worker, or lease is fail-closed (never MAC-only accept).
func WorkerVerifySealed(sealedPath, secret, task, worker string, lease int64) error {
	task = strings.TrimSpace(task)
	worker = strings.TrimSpace(worker)
	if task == "" {
		return fmt.Errorf("%w: expected task required for sealed verify", envelope.ErrMissingBinding)
	}
	if worker == "" {
		return fmt.Errorf("%w: expected live AgentSessionID required for sealed verify", envelope.ErrMissingBinding)
	}
	if strings.HasPrefix(worker, "pending-") {
		return fmt.Errorf("%w: provisional expected worker refused", envelope.ErrMissingBinding)
	}
	if lease <= 0 {
		return fmt.Errorf("%w: expected lease >0 required for sealed verify", envelope.ErrMissingFields)
	}
	st, err := LoadSealedControl(sealedPath, secret, task, worker, lease)
	if err != nil {
		return err
	}
	if st.Control == nil || st.Control.Signature == "" {
		return envelope.ErrInvalidSignature
	}
	ws := st.Control.TargetWorkerSession
	if ws == "" || strings.HasPrefix(ws, "pending-") {
		return fmt.Errorf("%w: sealed worker must be live AgentSessionID (not provisional)", envelope.ErrMissingBinding)
	}
	if worker != ws {
		return fmt.Errorf("%w: worker %q != sealed %q", envelope.ErrWorkerMismatch, worker, ws)
	}
	if st.Control.TargetTask != task {
		return fmt.Errorf("%w: task mismatch", envelope.ErrTaskMismatch)
	}
	if st.Control.LeaseGeneration != lease {
		return fmt.Errorf("%w: lease mismatch", envelope.ErrStaleGeneration)
	}
	return nil
}

// WorkerVerifySealedFile loads secret from adjacent shared control dir and
// requires exact task/worker/lease bindings (from args or environment).
func WorkerVerifySealedFile(sealedPath, task, worker string, lease int64) error {
	if task == "" {
		task = strings.TrimSpace(os.Getenv("HERD_EXPECTED_TASK"))
	}
	if worker == "" {
		worker = strings.TrimSpace(os.Getenv("HERD_EXPECTED_WORKER"))
	}
	if lease <= 0 {
		if v := strings.TrimSpace(os.Getenv("HERD_EXPECTED_LEASE")); v != "" {
			var n int64
			_, _ = fmt.Sscanf(v, "%d", &n)
			lease = n
		}
	}
	dir := filepath.Dir(filepath.Dir(sealedPath)) // .../control
	secret, err := os.ReadFile(filepath.Join(dir, "mac.secret"))
	if err != nil {
		return err
	}
	return WorkerVerifySealed(sealedPath, strings.TrimSpace(string(secret)), task, worker, lease)
}

// WaitForSealedControl polls until the barrier seal exists (start barrier).
func WaitForSealedControl(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if st, err := os.Stat(path); err == nil && st.Size() > 0 {
			return nil
		}
		time.Sleep(25 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for sealed control at %s", path)
}
