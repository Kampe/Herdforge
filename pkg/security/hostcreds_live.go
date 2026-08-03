package security

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/harness"
	"github.com/Kampe/Herdforge/pkg/herdr"
)

// LiveProof is exact-session evidence for a real hosted author OS process.
// Full admission also requires FAC-169 OS boundary (RequireOSBoundary).
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
}

// LiveConfig configures production live author launch.
type LiveConfig struct {
	Kind          string
	SessionID     string
	Prompt        string
	AllowedMarker string
	Authority     CredentialAuthority
	WorkDir       string
	UseHerdr      bool
	Workspace     string
	Timeout       time.Duration
}

// StartAuthorLive launches a real hosted author (grok|claude|codex) non-interactively
// through HostCreds MITM transport. OS isolation is owned by FAC-169 — this
// function fails closed until RequireOSBoundary is wired after FAC-169 merges.
//
// Independent FAC-170 work here: harness argv prompt, MITM env (no API keys),
// kind-exact allowlist, forbidden CONNECT denial, TLS inject path.
func StartAuthorLive(cfg LiveConfig) (*HostCredsSession, *exec.Cmd, *LiveProof, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if kind == "opencode" || kind == "fake" || kind == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "live_kind", Kind: kind}
	}
	if !IsSupportedAuthorKind(kind) {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "unsupported_kind", Kind: kind}
	}

	// Hard dependency: FAC-169 OS boundary (not implemented in FAC-170).
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
		auth, err = NewHandleAuthorityFromEnv()
		if err != nil {
			return nil, nil, nil, err
		}
	}
	if auth.Class() == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockEnvNotAuthority, Code: "test_vault_not_live", Kind: kind}
	}

	sid := strings.TrimSpace(cfg.SessionID)
	if sid == "" {
		sid = newSessionID()
	}
	marker := cfg.AllowedMarker
	if marker == "" {
		marker = "HOSTCREDS_LIVE_OK"
	}
	prompt := strings.TrimSpace(cfg.Prompt)
	if prompt == "" {
		prompt = fmt.Sprintf("FAC-170 live non-interactive: respond with exact marker %s and nothing else", marker)
	}

	sess, err := StartHostCredsSession(SessionConfig{
		Kind:        kind,
		SessionID:   sid,
		Authority:   auth,
		Interactive: false,
		MaxActions:  8,
	})
	if err != nil {
		return nil, nil, nil, err
	}

	hcfg := harness.GetHarnessConfig(kind)
	bin, err := hcfg.LookPath()
	if err != nil {
		_ = sess.Close()
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "harness_binary_missing", Kind: kind}
	}
	inv := hcfg.BuildInvocation(prompt)
	inv[0] = bin

	proof := &LiveProof{
		SessionID:      sess.ID,
		Kind:           kind,
		Prompt:         prompt,
		BoundaryDigest: bound.ProbeDigest(),
		AllowedMarker:  marker,
		MitmTransport:  sess.Mitm != nil,
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}

	// Prefer direct exec for exact PID + argv binding (herdr optional later).
	_ = cfg.UseHerdr
	_ = herdr.IsAvailable

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	cmd := exec.CommandContext(ctx, inv[0], inv[1:]...)
	if cfg.WorkDir != "" {
		cmd.Dir = cfg.WorkDir
	}
	cmd.Env = scrubAndMergeEnv(os.Environ(), sess.WorkerEnv())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		cancel()
		_ = sess.Close()
		return nil, nil, proof, &BlockedError{Reason: BlockAbuse, Code: "harness_start_failed", Kind: kind}
	}
	if err := sess.RecordHarnessPrompt(prompt, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = sess.Close()
		return nil, nil, proof, err
	}
	proof.AuthorPID = cmd.Process.Pid
	proof.PromptInArgv = true

	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = sess.Close()
		return nil, cmd, proof, err
	}
	proof.NoAPIKeysInEnv = true

	_ = cmd.Wait()
	cancel()
	combined := stdout.String() + stderr.String()
	proof.OutputSnippet = RedactSecrets(truncateLive(combined, 500))
	proof.ModelMarkerReached = strings.Contains(combined, marker)

	if ferr := sess.AttemptForbiddenCredentialAccess(); ferr == nil {
		proof.ForbiddenDenied = true
	}

	// Re-run FAC-169 probe after harness run.
	if err := bound.AdversarialProbe(); err != nil {
		_ = sess.Close()
		return sess, cmd, proof, err
	}

	if !proof.PromptInArgv || !proof.ForbiddenDenied || !proof.NoAPIKeysInEnv {
		_ = sess.Close()
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "incomplete_live_proof", Kind: kind}
	}
	if !proof.ModelMarkerReached {
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "marker_not_reached", Kind: kind}
	}
	return sess, cmd, proof, nil
}

func scrubAndMergeEnv(parent, worker []string) []string {
	deny := map[string]bool{
		"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true, "XAI_API_KEY": true,
		"HERD_HOST_CREDS": true, "HERD_HOSTCREDS_HANDLES": true,
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
		lk := strings.ToLower(k)
		if lk == "http_proxy" || lk == "https_proxy" || lk == "all_proxy" {
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
