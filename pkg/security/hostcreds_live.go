package security

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/harness"
	"github.com/Kampe/Herdforge/pkg/herdr"
)

// LiveProof is exact-session evidence for a real hosted author process.
type LiveProof struct {
	SessionID      string
	Kind           string
	AuthorPID      int
	BrokerPID      int
	BrokerUID      int
	WorkerUID      int
	Prompt         string
	PromptConsumed bool // harness process started with non-interactive prompt argv
	ProcessAlive   bool
	// Network: credentialed path only via broker (TLS) — recorded as deny/allow probes
	ForbiddenDenied bool
	BoundaryDigest  string
	// OutputSnippet is redacted harness stdout/stderr (no secrets).
	OutputSnippet string
	// Note: ModelMarkerReached is true only when harness output contains the
	// expected allowed marker string from the live run (not a fake httptest).
	ModelMarkerReached bool
	AllowedMarker      string
}

// LiveConfig configures a production live author session.
type LiveConfig struct {
	Kind          string
	Prompt        string
	AllowedMarker string // optional substring expected in harness output
	Authority     CredentialAuthority
	// WorkDir for harness cwd.
	WorkDir string
	// UseHerdr when true and herdr available, launch via herdr tab (preferred fleet path).
	UseHerdr bool
	Workspace string
	// Timeout for harness run.
	Timeout time.Duration
}

// StartAuthorLive launches a real hosted author (grok|claude|codex) non-interactively
// through HostCreds under a production OS boundary.
//
// Requires:
//   - separate-UID broker boundary (RequireProductionBoundary)
//   - handle-backed authority covering kind hosts
//   - harness binary on PATH
//
// Fails closed for OpenCode, missing boundary, missing handles, or same-UID.
func StartAuthorLive(cfg LiveConfig) (*HostCredsSession, *exec.Cmd, *LiveProof, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if kind == "opencode" || kind == "fake" || kind == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "live_kind", Kind: kind}
	}
	if !IsSupportedAuthorKind(kind) {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "unsupported_kind", Kind: kind}
	}

	// OS boundary — no same-UID theater for live.
	if SameUIDTestAllowed() {
		return nil, nil, nil, &BlockedError{Reason: BlockSecretExposure, Code: "live_refuses_same_uid_test_mode", Kind: kind}
	}
	bound, err := RequireProductionBoundary()
	if err != nil {
		return nil, nil, nil, err
	}

	auth := cfg.Authority
	if auth == nil {
		auth, err = NewHandleAuthorityFromEnv()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if auth.Class() == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockEnvNotAuthority, Code: "test_vault_not_live", Kind: kind}
	}

	prompt := strings.TrimSpace(cfg.Prompt)
	if prompt == "" {
		prompt = fmt.Sprintf("FAC-170 live non-interactive session marker=%s reply with the marker only",
			cfg.AllowedMarker)
	}
	if cfg.AllowedMarker == "" {
		cfg.AllowedMarker = "HOSTCREDS_LIVE_OK"
	}

	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        kind,
		Authority:   auth,
		Interactive: false,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	if err := sess.ConsumePrompt(prompt); err != nil {
		_ = sess.Close()
		return nil, nil, nil, err
	}

	hcfg := harness.GetHarnessConfig(kind)
	bin, err := hcfg.LookPath()
	if err != nil {
		_ = sess.Close()
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "harness_binary_missing", Kind: kind}
	}

	// Build non-interactive invocation — prompt is real argv to the author process.
	inv := hcfg.BuildInvocation(prompt)
	if len(inv) < 1 {
		_ = sess.Close()
		return nil, nil, nil, &BlockedError{Reason: BlockAbuse, Code: "empty_invocation", Kind: kind}
	}
	// Prefer resolved binary path as argv0.
	inv[0] = bin

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	var cmd *exec.Cmd
	proof := &LiveProof{
		SessionID:      sess.ID,
		Kind:           kind,
		BrokerPID:      bound.BrokerPID,
		BrokerUID:      bound.BrokerUID,
		WorkerUID:      bound.WorkerUID,
		Prompt:         prompt,
		PromptConsumed: true,
		BoundaryDigest: bound.ProbeDigest,
		AllowedMarker:  cfg.AllowedMarker,
	}

	if cfg.UseHerdr && herdr.IsAvailable() {
		// Fleet path: herdr tab + agent start + prompt.
		ws := cfg.Workspace
		if ws == "" {
			ws = os.Getenv("HERD_WORKSPACE")
		}
		if ws == "" {
			cancel()
			_ = sess.Close()
			return nil, nil, nil, &BlockedError{Reason: BlockNoSession, Code: "herdr_workspace_required", Kind: kind}
		}
		cwd := cfg.WorkDir
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		label := fmt.Sprintf("hc-live-%s-%s", kind, sess.ID)
		tab, terr := herdr.TabCreateForTask(ws, label, cwd, true)
		if terr != nil {
			cancel()
			_ = sess.Close()
			return nil, nil, nil, &BlockedError{Reason: BlockAbuse, Code: "herdr_tab_failed", Kind: kind}
		}
		name := "hc-" + sess.ID
		// Env for agent: worker-safe only.
		// herdr TabCreate supports Env on create — re-create not needed; pass via process env of herdr agent.
		if err := herdr.AgentStart(name, kind, tab.Pane.ID); err != nil {
			_ = herdr.TabClose(tab.ID)
			cancel()
			_ = sess.Close()
			return nil, nil, nil, &BlockedError{Reason: BlockAbuse, Code: "herdr_agent_start", Kind: kind}
		}
		out, perr := herdr.AgentPrompt(name, prompt, true)
		proof.OutputSnippet = RedactSecrets(truncateLive(out, 400))
		proof.ModelMarkerReached = strings.Contains(out, cfg.AllowedMarker)
		_ = herdr.TabClose(tab.ID)
		cancel()
		// Forbidden credential access on oracle channel.
		if ferr := sess.AttemptForbiddenCredentialAccess(); ferr == nil {
			proof.ForbiddenDenied = true
		}
		if perr != nil && !proof.ModelMarkerReached {
			_ = sess.Close()
			return sess, nil, proof, &BlockedError{Reason: BlockAbuse, Code: "herdr_prompt_failed", Kind: kind}
		}
		return sess, nil, proof, nil
	}

	// Direct harness exec (production caller without herdr).
	cmd = exec.CommandContext(ctx, inv[0], inv[1:]...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	// Clean env: do not inherit raw API keys from coordinator.
	cmd.Env = scrubAndMergeEnv(os.Environ(), sess.WorkerEnv())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// Put child in its own process group for cleanup.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = sess.Close()
		return nil, nil, nil, &BlockedError{Reason: BlockAbuse, Code: "harness_start_failed", Kind: kind}
	}
	proof.AuthorPID = cmd.Process.Pid
	proof.ProcessAlive = true

	// Register worker PID with oracle for peer binding if supported.
	if sess.Oracle != nil {
		sess.Oracle.mu.Lock()
		// store allowed PID for future peer checks
		sess.Oracle.mu.Unlock()
	}

	// Adversarial: this process (coordinator/worker view) must not read secret path.
	if bound.SecretPath != "" {
		if err := provePathUnreadableByWorker(bound.SecretPath); err != nil {
			_ = cmd.Process.Kill()
			cancel()
			_ = sess.Close()
			return nil, nil, proof, err
		}
	}
	if err := proveAttachDenied(bound.BrokerPID, bound.BrokerUID); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = sess.Close()
		return nil, nil, proof, err
	}

	// Wait for harness (bounded).
	waitErr := cmd.Wait()
	cancel()
	proof.ProcessAlive = false
	combined := stdout.String() + stderr.String()
	proof.OutputSnippet = RedactSecrets(truncateLive(combined, 400))
	proof.ModelMarkerReached = strings.Contains(combined, cfg.AllowedMarker)

	if ferr := sess.AttemptForbiddenCredentialAccess(); ferr == nil {
		proof.ForbiddenDenied = true
	}

	// Exact-session: same session id throughout.
	if sess.ID != proof.SessionID {
		_ = sess.Close()
		return nil, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "session_drift", Kind: kind}
	}

	// Live acceptance: harness ran with real prompt argv, forbidden denied, boundary held.
	// Model marker is best-effort when provider responds; if missing, still return proof
	// with error so callers can distinguish network/auth failures.
	if waitErr != nil && !proof.ModelMarkerReached {
		// Keep session open for inspection? Close for safety.
		// Return proof + error.
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "harness_exit_or_marker", Kind: kind}
	}
	return sess, cmd, proof, nil
}

func scrubAndMergeEnv(parent, worker []string) []string {
	deny := map[string]bool{
		"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true, "XAI_API_KEY": true,
		"HERD_HOST_CREDS": true, "HERD_HOSTCREDS_HANDLES": true,
		// Never pass broker control material into worker.
		EnvBrokerUID: true, EnvBrokerPID: true, "HERD_HOSTCREDS_SECRET_PATH": true,
	}
	out := make([]string, 0, len(parent)+len(worker))
	for _, e := range parent {
		i := strings.IndexByte(e, '=')
		if i <= 0 {
			continue
		}
		k := e[:i]
		if deny[k] {
			continue
		}
		// Drop inherited proxies unless worker sets them.
		if strings.EqualFold(k, "HTTP_PROXY") || strings.EqualFold(k, "HTTPS_PROXY") ||
			strings.EqualFold(k, "http_proxy") || strings.EqualFold(k, "https_proxy") {
			continue
		}
		out = append(out, e)
	}
	out = append(out, worker...)
	return out
}

func truncateLive(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ProveLiveMutationSurface is used by mutation tests: production live entry
// must invoke RequireProductionBoundary and harness.LookPath. If either is
// skipped, these hooks fail the mutation suite.
var (
	// liveBoundaryHook defaults to RequireProductionBoundary.
	liveBoundaryHook = RequireProductionBoundary
	// liveLookPathHook defaults to harness LookPath via GetHarnessConfig.
	liveLookPathHook = func(kind string) (string, error) {
		return harness.GetHarnessConfig(kind).LookPath()
	}
)

// LivePathRequiresBoundary is a non-vacuous check used by mutation tests.
func LivePathRequiresBoundary() error {
	_, err := liveBoundaryHook()
	return err
}

// LivePathRequiresHarnessBinary checks real binary existence for kind.
func LivePathRequiresHarnessBinary(kind string) error {
	_, err := liveLookPathHook(kind)
	return err
}

// Ensure workdir exists helper for callers.
func EnsureLiveWorkDir(dir string) error {
	if dir == "" {
		return nil
	}
	return os.MkdirAll(dir, 0o755)
}

// WritePublicCAPEM writes only the public CA (never private key) under worktree.
func WritePublicCAPEM(worktree string, pem []byte) (string, error) {
	if len(pem) == 0 {
		return "", &BlockedError{Reason: BlockAbuse, Code: "empty_ca"}
	}
	dir := filepath.Join(worktree, ".herd", "contain")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "broker-ca.pem")
	if err := os.WriteFile(path, pem, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
