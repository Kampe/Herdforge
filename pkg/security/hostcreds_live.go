package security

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/harness"
)

// LiveProof is exact-session evidence for a real hosted author OS process.
// Full admission also requires FAC-169 via RequireOSBoundary.
type LiveProof struct {
	SessionID          string
	Kind               string
	AuthorPID          int
	Prompt             string
	PromptInArgv       bool
	ForbiddenDenied    bool
	BoundaryDigest     string
	OutputSnippet      string
	ModelMarkerReached bool
	AllowedMarker      string
	MitmTransport      bool
	NoAPIKeysInEnv     bool
	IsolatedHOME       string
	BrokerReached      bool // MITM broker receipt inject for kind host
	WorkerProbeOK      bool // real worker full-TLS + redacted receipt
	ChildEnvProof      []string
	CapabilityNonce    string // non-secret nonce only
}

// LiveConfig configures production live author launch.
type LiveConfig struct {
	Kind      string
	SessionID string
	Prompt    string // optional; default CapabilityPrompt (never embeds Expected)
	// AllowedMarker is ignored for protocol — capability Expected is derived.
	// Kept for CLI compat; if set and non-empty must equal derived Expected or BLOCKED.
	AllowedMarker string
	Authority     CredentialAuthority
	WorkDir       string
	Timeout       time.Duration
}

// StartAuthorLive launches a real hosted author non-interactively through MITM.
//
// Wires FAC-170 Capability (non-echo marker) + worker-probe (forbidden deny +
// allow full TLS + broker redacted receipt). OS secret/UID boundary: FAC-169 only.
//
// Fail-closed: missing FAC-169, missing capability in response, missing harness
// binary, incomplete worker proof, empty API-key plant, etc.
func StartAuthorLive(cfg LiveConfig) (*HostCredsSession, *exec.Cmd, *LiveProof, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if kind == "opencode" || kind == "fake" || kind == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "live_kind", Kind: kind}
	}
	if !IsSupportedAuthorKind(kind) {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "unsupported_kind", Kind: kind}
	}

	bound, err := RequireOSBoundary()
	if err != nil {
		return nil, nil, nil, err
	}
	if bound == nil {
		return nil, nil, nil, &BlockedError{Reason: BlockSecretExposure, Code: "fac169_required", Kind: kind}
	}
	if err := bound.AdversarialProbe(); err != nil {
		return nil, nil, nil, err
	}

	// Production secrets must not be resolved into this process via op/security
	// until FAC-169 IPC owns authority. Refuse in-process handle resolution for live.
	auth := cfg.Authority
	if auth == nil {
		return nil, nil, nil, &BlockedError{
			Reason: BlockEnvNotAuthority,
			Code:   "live_authority_via_fac169_ipc_only",
			Kind:   kind,
		}
	}
	if auth.Class() == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockEnvNotAuthority, Code: "test_vault_not_live", Kind: kind}
	}
	if auth.Class() == "keychain" || auth.Class() == "op" || auth.Class() == "handle" {
		if _, ok := auth.(*HandleAuthority); ok {
			return nil, nil, nil, &BlockedError{
				Reason: BlockEnvNotAuthority,
				Code:   "in_process_handle_authority_not_live",
				Kind:   kind,
			}
		}
	}

	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}

	// Capability: Expected never appears in prompt (blocks echo-as-success).
	cap, err := NewCapability(sid)
	if err != nil {
		return nil, nil, nil, err
	}
	if m := strings.TrimSpace(cfg.AllowedMarker); m != "" && m != cap.Expected {
		return nil, nil, nil, &BlockedError{Reason: BlockAbuse, Code: "marker_not_capability", Kind: kind}
	}
	marker := cap.Expected
	prompt := strings.TrimSpace(cfg.Prompt)
	if prompt == "" {
		prompt = CapabilityPrompt(cap)
	}
	if strings.Contains(prompt, marker) {
		return nil, nil, nil, &BlockedError{Reason: BlockAbuse, Code: "marker_in_prompt", Kind: kind}
	}

	sess, err := StartHostCredsSession(SessionConfig{
		Kind: kind, SessionID: sid, Authority: auth, Interactive: false, MaxActions: 8,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	// Isolated HOME — no host ~/.codex / login files.
	homeDir, err := os.MkdirTemp("", "hc-home-*")
	if err != nil {
		_ = sess.Close()
		return nil, nil, nil, err
	}

	hcfg := harness.GetHarnessConfig(kind)
	bin, err := hcfg.LookPath()
	if err != nil {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "harness_binary_missing", Kind: kind}
	}
	inv := hcfg.BuildInvocation(prompt)
	inv[0] = bin

	proof := &LiveProof{
		SessionID:       sess.ID,
		Kind:            kind,
		Prompt:          prompt,
		BoundaryDigest:  bound.ProbeDigest(),
		AllowedMarker:   marker,
		CapabilityNonce: cap.Nonce,
		MitmTransport:   sess.Mitm != nil,
		IsolatedHOME:    homeDir,
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	cmd := exec.CommandContext(ctx, inv[0], inv[1:]...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	// Exact child env: scrubbed base + WorkerEnv + capability nonce + isolated HOME.
	// No append(os.Environ()) — no secret leftovers / duplicate keys.
	childEnv := ExactWorkerChildEnv(
		[]string{"PATH=" + os.Getenv("PATH"), "LANG=C", "HOME=" + homeDir},
		sess.WorkerEnv(),
		CapabilityEnv(cap),
		[]string{
			"CODEX_HOME=" + filepath.Join(homeDir, "codex-empty"),
		},
	)
	if err := assertExactEnvNoSecrets(childEnv); err != nil {
		cancel()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, &BlockedError{Reason: BlockSecretExposure, Code: "child_env:" + err.Error()}
	}
	cmd.Env = childEnv
	proof.ChildEnvProof = append([]string(nil), childEnv...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, &BlockedError{Reason: BlockAbuse, Code: "harness_start_failed", Kind: kind}
	}
	if err := sess.RecordHarnessPrompt(prompt, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, err
	}
	proof.AuthorPID = cmd.Process.Pid
	proof.PromptInArgv = true

	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, cmd, proof, err
	}
	for _, e := range childEnv {
		if strings.HasPrefix(e, "XAI_API_KEY=") && e != "XAI_API_KEY=" {
			_ = cmd.Process.Kill()
			cancel()
			_ = sess.Close()
			return nil, cmd, proof, &BlockedError{Reason: BlockSecretExposure, Code: "child_has_api_key"}
		}
		if strings.HasPrefix(e, "HOME=") && e != "HOME="+homeDir {
			_ = cmd.Process.Kill()
			cancel()
			_ = sess.Close()
			return nil, cmd, proof, &BlockedError{Reason: BlockSecretExposure, Code: "child_home_not_isolated"}
		}
	}
	proof.NoAPIKeysInEnv = true

	_ = cmd.Wait()
	cancel()
	combined := stdout.String() + stderr.String()
	proof.OutputSnippet = RedactSecrets(truncateLive(combined, 500))
	// Non-echo: Expected not in prompt; must appear in model output.
	proof.ModelMarkerReached = VerifyCapabilityOutput(cap, prompt, combined)

	// Worker-executed forbidden + allow full TLS + broker redacted receipt.
	// Not coordinator-side AttemptForbiddenCredentialAccess alone.
	wres, werr := sess.RunWorkerForbiddenAndAllowProbe(cap.Nonce)
	if werr == nil && wres != nil {
		proof.ForbiddenDenied = strings.Contains(wres.DenyCONNECT, "403")
		proof.WorkerProbeOK = wres.TLSRequestOK && proof.ForbiddenDenied
	}
	if !proof.ForbiddenDenied {
		// Still record fail; do not fall back to coordinator-only deny as success.
		proof.ForbiddenDenied = false
		proof.WorkerProbeOK = false
	}

	// Broker reached: MITM redacted receipt for required host.
	if sess.Mitm != nil {
		required := RequiredBrokerHostsForKind(kind)
		sess.Mitm.mu.Lock()
		rcpt := sess.Mitm.LastReceipt
		sess.Mitm.mu.Unlock()
		if rcpt.InjectOK && len(required) > 0 && rcpt.Host == required[0] {
			proof.BrokerReached = true
		} else if rcpt.InjectOK && sess.Mitm.LastInjectHost != "" {
			// accept inject host match
			for _, h := range required {
				if sess.Mitm.LastInjectHost == h {
					proof.BrokerReached = true
					break
				}
			}
		}
	}

	if err := bound.AdversarialProbe(); err != nil {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return sess, cmd, proof, err
	}

	// Never succeed on bare exit 0 without full proof chain.
	if !proof.PromptInArgv || !proof.NoAPIKeysInEnv || !proof.ForbiddenDenied || !proof.WorkerProbeOK {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "incomplete_live_proof", Kind: kind}
	}
	if !proof.ModelMarkerReached {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "marker_not_reached", Kind: kind}
	}
	if !proof.BrokerReached {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "broker_not_reached", Kind: kind}
	}
	_ = os.RemoveAll(homeDir)
	return sess, cmd, proof, nil
}

// scrubAndMergeEnv is retained for tests that merge parent+worker; production
// live/worker paths use ExactWorkerChildEnv (no duplicate keys, no secret append).
func scrubAndMergeEnv(parent, worker []string) []string {
	return ExactWorkerChildEnv(parent, worker)
}

func truncateLive(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
