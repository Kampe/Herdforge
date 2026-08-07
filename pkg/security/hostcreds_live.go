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

// LiveProof is exact-session evidence for one author OS process.
// Full admission also requires FAC-169 via RequireOSBoundary + separate-UID IPC authority.
type LiveProof struct {
	SessionID          string
	Kind               string
	AuthorPID          int
	PeerPort           int
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
	BrokerReached      bool
	WorkerProbeOK      bool // true when AUTHOR process itself completed TLS+deny
	ReceiptDigest      string
	CapabilityNonce    string
	ChildEnvProof      []string
	HarnessWaitOK      bool // cmd.Wait succeeded or typed causal exit
	AuthorCausal       bool // proof from author process, not helper
	// ModelEvidence is true ONLY when the author was a real hosted harness.
	//
	// The CausalAuthorOnly path runs herd's own `hostcreds author-causal`
	// binary, which derives the capability marker with in-process SHA-256 and
	// prints it — so ModelMarkerReached there is herd verifying its own hash
	// against its own expectation. That is a transport proof (peer attribution
	// + host policy + broker inject + bound receipt), NOT proof a model ran.
	// Callers must not treat the two as interchangeable.
	ModelEvidence bool
}

// LiveConfig configures production live author launch.
type LiveConfig struct {
	Kind      string
	SessionID string
	Prompt    string
	// AllowedMarker ignored for protocol (capability Expected is derived).
	AllowedMarker string
	Authority     CredentialAuthority
	WorkDir       string
	Timeout       time.Duration
	// CausalAuthorOnly: for E2E/selftest, run author-causal as the author child
	// (exact one-shot FD peer). Real Grok/Claude/Codex require FAC-169 IPC +
	// attributable channel; without one-shot FD they fail closed on Darwin.
	CausalAuthorOnly bool
}

// StartAuthorLive launches a real hosted author non-interactively through MITM.
//
// Exact-session proof MUST come from the author child itself (marker + allow TLS
// + forbidden deny + bound receipt). A post-exit helper probe is forbidden as
// a substitute. Peer authority is one-shot inherited claim FD.
//
// FAC-169: OS boundary + non-test authority (no in-process HandleAuthority/Test vault).
//
// REACHABILITY (review finding 7 — stated, not worked around): there is
// currently NO configuration that both passes and constitutes model evidence.
//
//   - CausalAuthorOnly=false (the real-harness path the CLI uses): stock Grok/
//     Claude/Codex CLIs do not dial the one-shot inherited claim FD, so they
//     produce no bound receipt and the run ends in author_not_causal. Peer
//     attribution for a stock CLI needs FAC-169's attributable channel.
//   - CausalAuthorOnly=true: the "author" is herd's own `hostcreds
//     author-causal`, which derives the capability marker by in-process
//     SHA-256. It proves transport end to end but no model ran, so
//     ModelEvidence is false and the CLI refuses to print PASS.
//
// Packet AC 1 and AC 6 are therefore BLOCKED on FAC-169, not satisfied here.
// Do not "fix" this by defaulting CausalAuthorOnly to true — that would make
// the CLI print PASS for herd verifying its own hash.
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

	auth := cfg.Authority
	if auth == nil {
		return nil, nil, nil, &BlockedError{
			Reason: BlockEnvNotAuthority,
			Code:   "live_authority_via_fac169_ipc_only",
			Kind:   kind,
		}
	}
	// In-process authorities are never live-admissible (test vault, handle in-proc).
	if auth.Class() == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockEnvNotAuthority, Code: "test_vault_not_live", Kind: kind}
	}
	if _, ok := auth.(*HandleAuthority); ok {
		return nil, nil, nil, &BlockedError{
			Reason: BlockEnvNotAuthority,
			Code:   "in_process_handle_authority_not_live",
			Kind:   kind,
		}
	}
	if auth.Class() != "fac169-ipc" && auth.Class() != "fac169" {
		// Until FAC-169 IPC class exists, any non-fac169 class is rejected for live.
		return nil, nil, nil, &BlockedError{
			Reason: BlockEnvNotAuthority,
			Code:   "authority_not_fac169_ipc",
			Kind:   kind,
		}
	}

	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}
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

	homeDir, err := os.MkdirTemp("", "hc-home-*")
	if err != nil {
		_ = sess.Close()
		return nil, nil, nil, err
	}

	// One-shot peer for THIS author only (inherited FD — not claim file).
	port, claimFD, err := ClaimLocalPort()
	if err != nil {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, nil, err
	}
	if err := sess.Mitm.AllowOneShotPeer(PeerGrant{
		Port: port, SessionID: sid, CapabilityNonce: cap.Nonce,
	}); err != nil {
		_ = claimFD.Close()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, nil, err
	}

	proof := newLiveProof(cfg, kind, prompt, marker, sess, cap, bound, homeDir, port)

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)

	childEnv := liveChildEnv(sess, cap, homeDir)
	// The reported isolation must be the applied isolation.
	if envValue(childEnv, "HOME") != homeDir {
		cancel()
		_ = claimFD.Close()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, &BlockedError{Reason: BlockSecretExposure, Code: "home_isolation_not_applied"}
	}
	if err := assertExactEnvNoSecrets(childEnv); err != nil {
		cancel()
		_ = claimFD.Close()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, &BlockedError{Reason: BlockSecretExposure, Code: "child_env"}
	}

	var cmd *exec.Cmd
	var stdout, stderr bytes.Buffer
	outProof := filepath.Join(homeDir, "author-proof.json")

	if cfg.CausalAuthorOnly {
		// Exact-session causal author binary (same process: marker+TLS+deny).
		exe, eerr := findHerdOrBuild(homeDir)
		if eerr != nil {
			cancel()
			_ = claimFD.Close()
			_ = os.RemoveAll(homeDir)
			_ = sess.Close()
			return nil, nil, proof, eerr
		}
		allow := ""
		if len(sess.rules) > 0 {
			allow = sess.rules[0].Host
		}
		cmd = exec.CommandContext(ctx, exe,
			"hostcreds", "author-causal",
			"--proxy", sess.Mitm.ProxyURL(),
			"--allow-host", allow,
			"--deny-host", "evil.example.invalid",
			"--session", sid,
			"--nonce", cap.Nonce,
			"--out", outProof,
		)
	} else {
		// Real harness: must use one-shot claim FD itself (stock CLIs do not —
		// fail closed unless they produce bound receipt from PeerPort).
		hcfg := harness.GetHarnessConfig(kind)
		bin, lerr := hcfg.LookPath()
		if lerr != nil {
			cancel()
			_ = claimFD.Close()
			_ = os.RemoveAll(homeDir)
			_ = sess.Close()
			return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "harness_binary_missing", Kind: kind}
		}
		// BuildInvocationE, not BuildInvocation: the latter returns nil on a
		// compile error by design ("callers cannot accidentally launch an
		// uncompiled surface"), and inv[0] would turn that fail-closed nil into
		// a panic instead of a typed BlockedError.
		inv, ierr := hcfg.BuildInvocationE(prompt)
		if ierr != nil || len(inv) == 0 {
			cancel()
			_ = claimFD.Close()
			_ = os.RemoveAll(homeDir)
			_ = sess.Close()
			return nil, nil, nil, &BlockedError{
				Reason: BlockUnbrokerableKind, Code: "harness_invocation_uncompilable", Kind: kind,
			}
		}
		inv[0] = bin
		cmd = exec.CommandContext(ctx, inv[0], inv[1:]...)
		if cfg.WorkDir != "" {
			cmd.Dir = cfg.WorkDir
		}
	}
	cmd.Env = childEnv
	cmd.ExtraFiles = []*os.File{claimFD}
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	proof.ChildEnvProof = append([]string(nil), childEnv...)

	if err := cmd.Start(); err != nil {
		cancel()
		_ = claimFD.Close()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, &BlockedError{Reason: BlockAbuse, Code: "harness_start_failed", Kind: kind}
	}
	_ = claimFD.Close() // child holds ExtraFiles dup
	if err := sess.RecordHarnessPrompt(prompt, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, nil, proof, err
	}
	// Bind grant author PID after start (informational on receipt).
	proof.AuthorPID = cmd.Process.Pid
	proof.PromptInArgv = true

	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, cmd, proof, err
	}
	proof.NoAPIKeysInEnv = true

	// MUST observe Wait error — crash/login-fail cannot proceed to success.
	waitErr := cmd.Wait()
	cancel()
	combined := stdout.String() + stderr.String()
	proof.OutputSnippet = RedactSecrets(truncateLive(combined, 500))
	proof.HarnessWaitOK = waitErr == nil

	// Proof from AUTHOR only — never launch a helper after Wait.
	if cfg.CausalAuthorOnly {
		raw, rerr := os.ReadFile(outProof)
		if rerr != nil {
			_ = os.RemoveAll(homeDir)
			_ = sess.Close()
			return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "author_proof_missing", Kind: kind}
		}
		// Parse lightly for fields.
		if !strings.Contains(string(raw), `"tls_request_ok": true`) && !strings.Contains(string(raw), `"tls_request_ok":true`) {
			_ = os.RemoveAll(homeDir)
			_ = sess.Close()
			return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "author_tls_missing", Kind: kind}
		}
		if !strings.Contains(string(raw), "403") {
			_ = os.RemoveAll(homeDir)
			_ = sess.Close()
			return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "author_deny_missing", Kind: kind}
		}
		proof.ForbiddenDenied = true
		proof.WorkerProbeOK = true
		proof.AuthorCausal = true
	}

	proof.ModelMarkerReached = VerifyCapabilityOutput(cap, prompt, combined)

	// Bound receipt: must match this session + nonce + claimed peer port;
	// single consume. Receipt from a different peer (helper) cannot satisfy.
	rcpt, ok := sess.Mitm.ConsumeReceiptFor(sid, cap.Nonce, port, "")
	switch {
	case !ok || !rcpt.InjectOK || rcpt.PeerPort != port || rcpt.SessionID != sid:
		proof.BrokerReached = false
	case rcpt.AuthorPID != 0 && proof.AuthorPID != 0 && rcpt.AuthorPID != proof.AuthorPID:
		// Receipt stamped for a different process — reject.
		proof.BrokerReached = false
	default:
		proof.BrokerReached = true
		proof.ReceiptDigest = rcpt.RequestDigest
	}

	if err := bound.AdversarialProbe(); err != nil {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return sess, cmd, proof, err
	}

	if !proof.HarnessWaitOK {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "harness_wait_failed", Kind: kind}
	}
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
	if !proof.AuthorCausal && !cfg.CausalAuthorOnly {
		// Real harness without author-produced proof cannot pass.
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return nil, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "author_not_causal", Kind: kind}
	}
	_ = os.RemoveAll(homeDir)
	return sess, cmd, proof, nil
}

func scrubAndMergeEnv(parent, worker []string) []string {
	return ExactWorkerChildEnv(parent, worker)
}

// newLiveProof builds the initial LiveProof for a run.
//
// Extracted so the ModelEvidence derivation is observable by a test. Asserting
// it through StartAuthorLive is impossible: every authority a test can supply
// is rejected by the fac169-ipc gate, which returns before this struct is ever
// built, so a test driving StartAuthorLive cannot see the field at all.
//
// ModelEvidence is the inverse of CausalAuthorOnly. The causal path runs herd's
// own `hostcreds author-causal`, which derives the capability marker with
// in-process SHA-256 and prints it — ModelMarkerReached there is herd checking
// its own hash, a transport proof, not proof a model ran.
func newLiveProof(
	cfg LiveConfig,
	kind, prompt, marker string,
	sess *HostCredsSession,
	cap Capability,
	bound OSBoundary,
	homeDir string,
	port int,
) *LiveProof {
	return &LiveProof{
		SessionID:       sess.ID,
		Kind:            kind,
		Prompt:          prompt,
		BoundaryDigest:  bound.ProbeDigest(),
		AllowedMarker:   marker,
		CapabilityNonce: cap.Nonce,
		MitmTransport:   sess.Mitm != nil,
		IsolatedHOME:    homeDir,
		PeerPort:        port,
		AuthorCausal:    cfg.CausalAuthorOnly,
		// Self-test author ⇒ transport proof only, never model evidence.
		ModelEvidence: !cfg.CausalAuthorOnly,
	}
}

// liveChildEnv composes the exact environ handed to a live author child.
//
// Extracted so the ordering is testable: StartAuthorLive itself is unreachable
// until FAC-169 lands, so a bug in this composition could not otherwise be
// caught by any test.
//
// HOME must be in the LAST group. ExactWorkerChildEnv is last-key-wins and
// sess.WorkerEnv() (HarnessProxyEnv) carries a blanket "HOME=" to hide host
// auth files. Supplying the isolated HOME in the first group left the child
// with an empty HOME while LiveProof.IsolatedHOME advertised the temp dir —
// evidence for an isolation that was never applied.
func liveChildEnv(sess *HostCredsSession, cap Capability, homeDir string) []string {
	return ExactWorkerChildEnv(
		[]string{"PATH=" + os.Getenv("PATH"), "LANG=C"},
		sess.WorkerEnv(),
		CapabilityEnv(cap),
		[]string{
			"HOME=" + homeDir,
			"CODEX_HOME=" + filepath.Join(homeDir, "codex-empty"),
			"HERD_HOSTCREDS_CLAIM_FD=3",
		},
	)
}

// envValue returns the effective value of key in an environ slice, honouring
// last-key-wins (the same rule ExactWorkerChildEnv applies).
func envValue(env []string, key string) string {
	out := ""
	for _, e := range env {
		if i := strings.IndexByte(e, '='); i > 0 && e[:i] == key {
			out = e[i+1:]
		}
	}
	return out
}

func truncateLive(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
