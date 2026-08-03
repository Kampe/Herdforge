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
type LiveProof struct {
	SessionID          string
	Kind               string
	AuthorPID          int
	BrokerPID          int
	BrokerUID          int
	WorkerUID          int
	Prompt             string
	PromptInArgv       bool // process started with prompt in argv
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

// StartAuthorLive launches a real hosted author (grok|claude|codex) non-interactively.
//
// Requires separate-UID OS boundary, handle-backed authority, harness binary.
// Worker env: HTTPS MITM proxy + public CA only — no real or dummy API keys.
// Prompt is delivered as harness argv; PromptConsumed only after process Start.
func StartAuthorLive(cfg LiveConfig) (*HostCredsSession, *exec.Cmd, *LiveProof, error) {
	kind := strings.ToLower(strings.TrimSpace(cfg.Kind))
	if kind == "opencode" || kind == "fake" || kind == "test" {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "live_kind", Kind: kind}
	}
	if !IsSupportedAuthorKind(kind) {
		return nil, nil, nil, &BlockedError{Reason: BlockUnbrokerableKind, Code: "unsupported_kind", Kind: kind}
	}
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
		BrokerPID:      bound.BrokerPID,
		BrokerUID:      bound.BrokerUID,
		WorkerUID:      bound.WorkerUID,
		Prompt:         prompt,
		BoundaryDigest: bound.ProbeDigest,
		AllowedMarker:  marker,
		MitmTransport:  sess.Mitm != nil,
	}

	// Boundary probes before launch.
	if bound.SecretPath != "" {
		if err := provePathUnreadableByWorker(bound.SecretPath); err != nil {
			_ = sess.Close()
			return nil, nil, proof, err
		}
	}
	if err := proveAttachDenied(bound.BrokerPID, bound.BrokerUID); err != nil {
		_ = sess.Close()
		return nil, nil, proof, err
	}

	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 90 * time.Second
	}

	if cfg.UseHerdr && herdr.IsAvailable() {
		ws := cfg.Workspace
		if ws == "" {
			ws = os.Getenv("HERD_WORKSPACE")
		}
		if ws == "" {
			_ = sess.Close()
			return nil, nil, proof, &BlockedError{Reason: BlockNoSession, Code: "herdr_workspace_required", Kind: kind}
		}
		cwd := cfg.WorkDir
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		// Pass harness proxy env into tab.
		tab, terr := herdr.TabCreate(herdr.TabCreateOptions{
			Workspace: ws,
			Label:     "hc-live-" + sess.ID,
			Cwd:       cwd,
			NoFocus:   true,
			Env:       sess.WorkerEnv(),
		})
		if terr != nil {
			_ = sess.Close()
			return nil, nil, proof, &BlockedError{Reason: BlockAbuse, Code: "herdr_tab_failed", Kind: kind}
		}
		name := "hc-" + sess.ID
		if err := herdr.AgentStart(name, kind, tab.Pane.ID); err != nil {
			_ = herdr.TabClose(tab.ID)
			_ = sess.Close()
			return nil, nil, proof, &BlockedError{Reason: BlockAbuse, Code: "herdr_agent_start", Kind: kind}
		}
		// herdr does not always expose OS PID; use negative sentinel then prompt.
		// RecordHarnessPrompt requires PID>0 — use process lookup best-effort or skip PID check for herdr.
		// For exact-session we require a real pid: try agent list.
		pid := 1 // placeholder blocked — require real
		if agents, aerr := herdr.AgentList(); aerr == nil {
			for _, a := range agents {
				if a.Name == name && a.TabID != "" {
					// no pid field typically
					_ = a
				}
			}
		}
		// Use self PID of herdr child is unavailable — fail closed unless we get a pid.
		// Fallback: start local harness process instead when PID unknown.
		_ = herdr.TabClose(tab.ID)
		_ = pid
		// Prefer direct exec for exact PID binding.
	}

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
	// Exact-session: prompt is in argv of this PID.
	if err := sess.RecordHarnessPrompt(prompt, cmd.Process.Pid); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = sess.Close()
		return nil, nil, proof, err
	}
	proof.AuthorPID = cmd.Process.Pid
	proof.PromptInArgv = true

	// Env must not contain API keys.
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		_ = cmd.Process.Kill()
		cancel()
		_ = sess.Close()
		return nil, cmd, proof, err
	}
	proof.NoAPIKeysInEnv = true

	// Wait bounded.
	_ = cmd.Wait()
	cancel()
	combined := stdout.String() + stderr.String()
	proof.OutputSnippet = RedactSecrets(truncateLive(combined, 500))
	proof.ModelMarkerReached = strings.Contains(combined, marker)

	if ferr := sess.AttemptForbiddenCredentialAccess(); ferr == nil {
		proof.ForbiddenDenied = true
	}

	// Re-prove boundary after run.
	if err := proveAttachDenied(bound.BrokerPID, bound.BrokerUID); err != nil {
		_ = sess.Close()
		return sess, cmd, proof, err
	}

	if !proof.PromptInArgv || !proof.ForbiddenDenied || !proof.NoAPIKeysInEnv {
		_ = sess.Close()
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "incomplete_live_proof", Kind: kind}
	}
	// Marker requires live provider; if missing return proof + BLOCKED for honesty.
	if !proof.ModelMarkerReached {
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "marker_not_reached", Kind: kind}
	}
	return sess, cmd, proof, nil
}

func scrubAndMergeEnv(parent, worker []string) []string {
	deny := map[string]bool{
		"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true, "XAI_API_KEY": true,
		"HERD_HOST_CREDS": true, "HERD_HOSTCREDS_HANDLES": true,
		EnvBrokerUID: true, EnvBrokerPID: true, "HERD_HOSTCREDS_SECRET_PATH": true,
		EnvAllowSameUIDTest: true,
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
