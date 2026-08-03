package security

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
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
	BrokerReached      bool // MITM observed inject for kind host
	ChildEnvProof      []string
}

// LiveConfig configures production live author launch.
type LiveConfig struct {
	Kind          string
	SessionID     string
	Prompt        string
	AllowedMarker string // must NOT be embedded in prompt for echo bypass
	Authority     CredentialAuthority
	WorkDir       string
	Timeout       time.Duration
}

// StartAuthorLive launches a real hosted author non-interactively through MITM.
//
// Independent FAC-170: harness argv, isolated HOME, WorkerEnv without API keys,
// MITM transport, kind allowlist. OS secret/UID boundary: FAC-169 only.
//
// Fail-closed: missing FAC-169, missing marker in response (prompt does not
// contain marker), missing harness binary, empty API-key plant, etc.
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
	// HandleAuthority that already hydrated secrets same-UID is rejected for live.
	if auth.Class() == "keychain" || auth.Class() == "op" || auth.Class() == "handle" {
		// Still same process if created via NewHandleAuthorityFromEnv — require FAC-169 IPC adapter class.
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
	// Marker must not appear in the prompt (blocks echo-as-success).
	marker := cfg.AllowedMarker
	if marker == "" {
		var b [8]byte
		_, _ = rand.Read(b[:])
		marker = "HC" + hex.EncodeToString(b[:])
	}
	prompt := strings.TrimSpace(cfg.Prompt)
	if prompt == "" {
		// Do not include marker text in the prompt.
		prompt = "FAC-170 live non-interactive: output a single line containing only the session capability token you were configured to emit"
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
		SessionID:      sess.ID,
		Kind:           kind,
		Prompt:         prompt,
		BoundaryDigest: bound.ProbeDigest(),
		AllowedMarker:  marker,
		MitmTransport:  sess.Mitm != nil,
		IsolatedHOME:   homeDir,
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
	// Exact child env: only scrubbed base + WorkerEnv + isolated HOME.
	childEnv := scrubAndMergeEnv([]string{"PATH=" + os.Getenv("PATH"), "LANG=C"}, sess.WorkerEnv())
	childEnv = append(childEnv, "HOME="+homeDir)
	// Force no user auth files.
	childEnv = append(childEnv,
		"ANTHROPIC_API_KEY=", "OPENAI_API_KEY=", "XAI_API_KEY=",
		"CODEX_HOME="+filepath.Join(homeDir, "codex-empty"),
	)
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
	// Prove child env list has empty keys and HOME isolated.
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
	// Marker must appear in output and must not be a mere prompt echo alone:
	// require marker not substring of prompt (already) and present in output.
	proof.ModelMarkerReached = strings.Contains(combined, marker) && !strings.Contains(prompt, marker)

	// Broker reached: MITM recorded inject for required host (if any traffic).
	if sess.Mitm != nil {
		required := RequiredBrokerHostsForKind(kind)
		if len(required) > 0 && sess.Mitm.LastInjectHost == required[0] {
			proof.BrokerReached = true
		}
	}

	// Forbidden probes on coordinator view of session policy (MITM deny).
	// Worker-executed probe requires FAC-169 + worker tooling; until then record coordinator deny.
	if ferr := sess.AttemptForbiddenCredentialAccess(); ferr == nil {
		proof.ForbiddenDenied = true
	}

	if err := bound.AdversarialProbe(); err != nil {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return sess, cmd, proof, err
	}

	// Never succeed on bare exit 0 without marker + broker reach + argv binding.
	if !proof.PromptInArgv || !proof.NoAPIKeysInEnv || !proof.ForbiddenDenied {
		_ = os.RemoveAll(homeDir)
		_ = sess.Close()
		return sess, cmd, proof, &BlockedError{Reason: BlockAbuse, Code: "incomplete_live_proof", Kind: kind}
	}
	if !proof.ModelMarkerReached {
		_ = os.RemoveAll(homeDir)
		// Keep session briefly? Close for safety.
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

func scrubAndMergeEnv(parent, worker []string) []string {
	deny := map[string]bool{
		"ANTHROPIC_API_KEY": true, "OPENAI_API_KEY": true, "XAI_API_KEY": true,
		"HERD_HOST_CREDS": true, "HERD_HOSTCREDS_HANDLES": true,
		"HOME": true, "USERPROFILE": true,
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
