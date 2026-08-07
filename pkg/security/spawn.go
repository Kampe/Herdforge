package security

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/envelope"
)

// ProcessSpawner is the process-boundary adapter (herdr in production).
type ProcessSpawner interface {
	CreateTab(workspace, label, cwd string, env []string, noFocus bool) (tabID, paneID string, err error)
	StartAgent(name, kind, paneID string, agentArgs []string) error
	CloseTab(tabID string) error
}

// AgentSpawnRequest is the mandatory production launch contract (FAC-133).
type AgentSpawnRequest struct {
	Policy    *LaunchPolicy
	Grant     *LaunchGrant
	Name      string
	Kind      string
	Model     string
	Workspace string
	Label     string
	NoFocus   bool
	Ambient   map[string]string
	// EventLogPath is required (production: $REPO/.herd/security-events.jsonl).
	EventLogPath string
	AgentArgs    []string
	// TaskRef and LeaseGeneration are required for durable session binding.
	TaskRef         string
	LeaseGeneration string
	// ClaimLookup proves lease against live FAC-147 claim/fence (required for tasks).
	ClaimLookup LiveClaimLookup
	// OwnerHint optional claim owner for live validation.
	OwnerHint string
	// SessionResolver is required when !SkipContainment to read live agent_session.
	SessionResolver LiveAgentResolver
	// SkipContainment is FORBIDDEN in production.
	SkipContainment bool
	// HostCreds are coordinator-only Authorization values for the broker
	// (never copied into agent env). When empty, CoordinatorHostCredsFromEnv is used.
	HostCreds map[string]string
	// PreboundControl is a MAC-signed scope correction that MUST be verified
	// and applied to Policy/Grant BEFORE containment Install (FAC-133 root
	// admission). Empty PackageAllowlist with Exclusive is refused.
	PreboundControl *envelope.Envelope
	// ControlSecret verifies PreboundControl (defaults to Policy control secret).
	ControlSecret string
	// PreStartSeal runs after CreateTab and BEFORE StartAgent. Required when
	// HERD_SEAL_WAIT=1: the wrapper blocks until the barrier seal exists, so
	// the seal MUST be written with a pre-start identity (herdr-pane:…) before
	// StartAgent. Returning after StartAgent deadlocks (CLI never execs → no
	// agent_session → seal never written). Returns the worker session id
	// published into the env file for exact wrapper verify.
	PreStartSeal func(tabID, paneID string) (workerSession string, err error)
}

// AgentSpawnResult is the launched agent identity + enforced surface.
type AgentSpawnResult struct {
	TabID          string
	PaneID         string
	Name           string
	Cwd            string
	Env            []string
	Role           string
	Network        string
	Containment    string
	ProfilePath    string
	WrapperPath    string
	ProvedDenials  bool
	Generation     string
	PolicyDigest   string
	AgentSessionID string
	Broker         *HostAllowBroker // owned; close with agent/tab lifetime
}

// LaunchAgent is the single mandatory production API for starting any agent.
func LaunchAgent(sp ProcessSpawner, req AgentSpawnRequest) (*AgentSpawnResult, error) {
	if sp == nil {
		return nil, fmt.Errorf("%w: nil process spawner", ErrUnknownPolicy)
	}
	if req.Policy == nil || req.Grant == nil {
		return nil, fmt.Errorf("%w: policy and grant required", ErrUnknownPolicy)
	}
	// SkipContainment is FORBIDDEN outside go test (matches doc claim).
	// Attestations with Containment "skipped" are also rejected by
	// RequireSessionAttestation / ValidateFleetAttestation.
	if req.SkipContainment && !testing.Testing() {
		return nil, fmt.Errorf("%w: SkipContainment is forbidden outside tests", ErrUnknownPolicy)
	}
	if strings.TrimSpace(req.EventLogPath) == "" {
		return nil, fmt.Errorf("%w: EventLogPath required", ErrUnknownPolicy)
	}
	if err := ValidateTaskRef(req.TaskRef); err != nil {
		return nil, err
	}
	standingOK := strings.EqualFold(strings.TrimSpace(req.TaskRef), "standing") ||
		strings.HasPrefix(strings.TrimSpace(req.TaskRef), "standing:")
	if err := ValidateLeaseGeneration(req.LeaseGeneration, standingOK); err != nil {
		return nil, err
	}
	if err := ValidateLiveTaskLease(context.Background(), req.ClaimLookup, req.TaskRef, req.LeaseGeneration, standingOK, req.OwnerHint, ""); err != nil {
		return nil, err
	}
	if err := BindDurableEvents(req.Policy, req.EventLogPath, memoryFrom(req.Policy.Events)); err != nil {
		return nil, err
	}
	if err := EnsureObservableEvents(req.Policy); err != nil {
		return nil, err
	}
	if err := req.Policy.Validate(); err != nil {
		_ = req.Policy.RecordFatal(EventPolicyBlock, err.Error(), "LaunchAgent")
		return nil, err
	}
	if req.Grant.CWD != req.Policy.FilesystemRoot {
		err := fmt.Errorf("%w: grant/policy cwd mismatch", ErrPathDenied)
		_ = req.Policy.RecordFatal(EventDenial, err.Error(), req.Grant.CWD)
		return nil, err
	}
	if err := req.Policy.AuthorizeCWD(req.Grant.CWD); err != nil {
		_ = req.Policy.RecordFatal(EventDenial, err.Error(), req.Grant.CWD)
		return nil, err
	}
	for _, tool := range req.Grant.AllowedTools {
		if err := req.Policy.AuthorizeTool(tool); err != nil {
			_ = req.Policy.RecordFatal(EventDenial, err.Error(), tool)
			return nil, err
		}
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Kind) == "" {
		return nil, fmt.Errorf("%w: agent name and kind required", ErrUnknownPolicy)
	}
	if strings.TrimSpace(req.Workspace) == "" {
		return nil, fmt.Errorf("%w: workspace required", ErrUnknownPolicy)
	}
	label := req.Label
	if label == "" {
		label = req.Name
	}

	// Limited network: durable broker is mandatory (survives CLI exit).
	var brokerLaunch *BrokerLaunch
	cleanupBroker := func() error {
		if brokerLaunch == nil {
			return nil
		}
		err := brokerLaunch.Close()
		brokerLaunch = nil
		return err
	}
	if !req.SkipContainment && req.Grant != nil && strings.EqualFold(req.Grant.Network, "limited") {
		hosts := req.Policy.NetworkAllowHosts
		if len(hosts) == 0 {
			hosts = DefaultHarnessAllowHosts()
		}
		provKey := "prov-" + sanitizeAgentName(req.Name)
		bl, berr := StartBrokerForLaunch(req.Policy.SharedCheckout, provKey, req.TaskRef+"/"+req.LeaseGeneration, hosts)
		if berr != nil {
			_ = req.Policy.RecordFatal(EventPolicyBlock, "broker_start_failed", berr.Error())
			return nil, fmt.Errorf("network broker start: %w", berr)
		}
		brokerLaunch = bl
		req.Policy.BrokerEndpoint = bl.Endpoint
		if req.Ambient == nil {
			req.Ambient = map[string]string{}
		}
		req.Ambient["HERD_NETWORK_BROKER"] = bl.ProxyURL
		req.Ambient["HTTP_PROXY"] = bl.ProxyURL
		req.Ambient["HTTPS_PROXY"] = bl.ProxyURL
		req.Ambient["http_proxy"] = bl.ProxyURL
		req.Ambient["https_proxy"] = bl.ProxyURL
		// Coordinator HostCreds + public CA for agent TLS trust (FAC-133).
		// Secrets stay on the broker; agent only gets SSL_CERT_FILE path.
		creds := req.HostCreds
		if len(creds) == 0 {
			creds = CoordinatorHostCredsFromEnv()
		}
		if caPath, cerr := WireBrokerHostCredsAndCA(bl, req.Grant.CWD, creds); cerr != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, "host_creds_wire_failed", cerr.Error())
			return nil, fmt.Errorf("network broker host creds: %w", cerr)
		} else if caPath != "" {
			req.Ambient["SSL_CERT_FILE"] = caPath
		}
		if bl.Inline != nil {
			if perr := ProveBrokerAllowDeny(bl.Inline, hosts[0], "evil.example"); perr != nil {
				_ = cleanupBroker()
				_ = req.Policy.RecordFatal(EventPolicyBlock, "broker_proof_failed", perr.Error())
				return nil, fmt.Errorf("network broker proof: %w", perr)
			}
		} else if perr := ProveDurableBrokerDeny(bl.Endpoint, bl.ProxyURL, "evil.example"); perr != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, "broker_proof_failed", perr.Error())
			return nil, fmt.Errorf("network broker proof: %w", perr)
		}
	}

	env, err := ConstructAgentEnv(req.Grant, req.Policy, req.Ambient)
	if err != nil {
		_ = cleanupBroker()
		_ = req.Policy.RecordFatal(EventDenial, err.Error(), "ConstructAgentEnv")
		return nil, err
	}
	if EnvHasSecret(env, req.Policy.SecretDeny...) {
		_ = cleanupBroker()
		err := fmt.Errorf("%w: constructed env leaked secret", ErrSecretPresent)
		_ = req.Policy.RecordFatal(EventDenial, err.Error(), "env_leak")
		return nil, err
	}

	// FAC-133: MAC-verify and apply control scope BEFORE Install so the
	// seatbelt profile encodes exclusive packages (causal kernel boundary).
	if req.PreboundControl != nil {
		secret := req.ControlSecret
		if secret == "" && req.Policy != nil {
			secret = req.Policy.ControlSecret
		}
		shared := ""
		if req.Policy != nil {
			shared = req.Policy.SharedCheckout
		}
		st, eerr := VerifyAndEnforceControl(secret, req.PreboundControl, req.Policy, req.Grant, req.Grant.CWD, shared)
		if eerr != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, "prebound_control_failed", eerr.Error())
			return nil, fmt.Errorf("prebound control enforce before install: %w", eerr)
		}
		// Point wrapper at sealed control for pre-sandbox re-verify (path only).
		if shared != "" && st != nil {
			if req.Ambient == nil {
				req.Ambient = map[string]string{}
			}
			req.Ambient["HERD_SEALED_CONTROL"] = SealedControlPath(shared, st.Task, st.WorkerSession)
		}
		// Re-construct env after package roots may have changed.
		env, err = ConstructAgentEnv(req.Grant, req.Policy, req.Ambient)
		if err != nil {
			_ = cleanupBroker()
			return nil, err
		}
	}

	var backend ContainmentBackend
	var profilePath, wrapperBin string
	proved := false
	if !req.SkipContainment {
		backend, err = RequireContainment()
		if err != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, err.Error(), "RequireContainment")
			return nil, err
		}
		realBin, rerr := ResolveAgentBinary(req.Kind)
		if rerr != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, "agent_binary_unresolved", rerr.Error())
			return nil, fmt.Errorf("resolve agent binary: %w", rerr)
		}
		pathPrefix, prof, envFile, ierr := backend.Install(req.Grant.CWD, req.Policy, req.Grant, req.Kind, realBin)
		if ierr != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, ierr.Error(), "Containment.Install")
			return nil, ierr
		}
		profilePath = prof
		wrapperBin = filepath.Join(pathPrefix, req.Kind)
		env = prependPATH(env, pathPrefix)
		if envFile == "" {
			envFile = filepath.Join(req.Grant.CWD, ".herd", "contain", "env.list")
		}
		if err := WriteEnvFile(envFile, env); err != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventDenial, err.Error(), "WriteEnvFile")
			return nil, err
		}
		if err := backend.ProveDenials(req.Grant.CWD, req.Policy, req.Grant, profilePath, env); err != nil {
			_ = cleanupBroker()
			_ = req.Policy.RecordFatal(EventPolicyBlock, err.Error(), "ProveDenials")
			return nil, err
		}
		proved = true
		_ = req.Policy.RecordFatal(EventKind("containment"), "denials_proved", backend.Name())
	} else {
		_ = req.Policy.RecordFatal(EventPolicyBlock, "containment_skipped_test_only", "")
	}

	tabID, paneID, err := sp.CreateTab(req.Workspace, label, req.Grant.CWD, env, req.NoFocus)
	if err != nil {
		_ = cleanupBroker()
		_ = req.Policy.RecordFatal(EventDenial, "tab_create_failed", err.Error())
		return nil, fmt.Errorf("sandbox launch tab create: %w", err)
	}
	closeTab := func() error {
		var first error
		if err := sp.CloseTab(tabID); err != nil {
			first = err
		}
		if err := CloseTabBroker(tabID); err != nil && first == nil {
			first = err
		}
		if err := CloseTabBrokerAt(req.Policy.SharedCheckout, tabID); err != nil && first == nil {
			first = err
		}
		if err := cleanupBroker(); err != nil && first == nil {
			first = err
		}
		return first
	}

	partial := &AgentSpawnResult{
		TabID: tabID, PaneID: paneID, Name: req.Name, Cwd: req.Grant.CWD,
		Env: env, Role: req.Grant.Role, Network: req.Grant.Network,
		ProfilePath: profilePath, WrapperPath: wrapperBin, ProvedDenials: proved,
		Broker: nil, // durable; see .herd/brokers state
	}
	if backend != nil {
		partial.Containment = backend.Name()
	} else if req.SkipContainment {
		partial.Containment = "skipped"
	} else {
		partial.Containment = "none"
	}

	// Causal start barrier: if HERD_SEAL_WAIT is set, seal MUST exist before
	// StartAgent so the wrapper can proceed. Pre-start identity is pane-bound
	// (herdr-pane:…); live agent_session may refine later. Without PreStartSeal
	// under seal-wait, fail closed (would deadlock).
	sealWait := req.Ambient != nil && req.Ambient["HERD_SEAL_WAIT"] == "1"
	if sealWait {
		if req.PreStartSeal == nil {
			_ = closeTab()
			return nil, fmt.Errorf("%w: HERD_SEAL_WAIT requires PreStartSeal before StartAgent (deadlock prevention)", ErrUnknownPolicy)
		}
		workerSess, serr := req.PreStartSeal(tabID, paneID)
		if serr != nil {
			_ = closeTab()
			return nil, fmt.Errorf("pre-start seal: %w", serr)
		}
		if strings.TrimSpace(workerSess) == "" || strings.HasPrefix(workerSess, "pending-") {
			_ = closeTab()
			return nil, fmt.Errorf("%w: pre-start seal returned provisional/empty worker", ErrSessionUnattested)
		}
		envFile := filepath.Join(req.Grant.CWD, ".herd", "contain", "env.list")
		if err := UpsertEnvFileKeys(envFile, map[string]string{
			"HERD_EXPECTED_WORKER": workerSess,
		}); err != nil {
			_ = closeTab()
			return nil, fmt.Errorf("pre-start publish worker: %w", err)
		}
		// Provisional session id for partial result until post-start resolve.
		partial.AgentSessionID = workerSess
	}

	agentArgs := append([]string(nil), req.AgentArgs...)
	if req.Model != "" {
		hasModel := false
		for _, a := range agentArgs {
			if a == "--model" {
				hasModel = true
				break
			}
		}
		if !hasModel {
			agentArgs = append([]string{"--model", req.Model}, agentArgs...)
		}
	}
	if err := sp.StartAgent(req.Name, req.Kind, paneID, agentArgs); err != nil {
		closeErr := sp.CloseTab(tabID)
		_ = cleanupBroker()
		_ = req.Policy.RecordFatal(EventDenial, "agent_start_failed", err.Error())
		if closeErr != nil {
			// Return partial tab identity so callers can re-attempt orphan close.
			return partial, fmt.Errorf("sandbox launch agent start: %w; tab close: %v", err, closeErr)
		}
		return nil, fmt.Errorf("sandbox launch agent start: %w", err)
	}

	// Resolve live identity after start. Session is optional provenance (grok
	// never reports one). Never invent ses_spawn_* / test-session-* ids.
	// Prefer real agent_session when present; else live-agent:name|tab|pane.
	agentSessionID := ""
	if req.SessionResolver != nil {
		var live *LiveAgentIdentity
		var lerr error
		deadline := time.Now().Add(15 * time.Second)
		for {
			live, lerr = req.SessionResolver.Lookup(req.Name)
			// Ready when: live agent present with tab/pane, or with a non-provisional session.
			if lerr == nil && live != nil {
				if live.TabID != "" {
					tabID = live.TabID
					partial.TabID = tabID
				}
				if live.PaneID != "" {
					paneID = live.PaneID
					partial.PaneID = paneID
				}
				sid := strings.TrimSpace(live.AgentSessionID)
				if sid != "" && !strings.HasPrefix(sid, "pending-") &&
					!strings.HasPrefix(sid, "ses_spawn_") &&
					!strings.HasPrefix(sid, "test-session-") {
					agentSessionID = sid
					break
				}
				// Session-less kinds: name+tab+pane is enough once listed.
				if live.Name != "" && tabID != "" && paneID != "" {
					break
				}
			}
			if time.Now().After(deadline) {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if lerr != nil && live == nil {
			if !req.SkipContainment {
				cErr := closeTab()
				err := fmt.Errorf("%w: live agent missing after start: %v", ErrSessionUnattested, lerr)
				_ = req.Policy.RecordFatal(EventPolicyBlock, "agent_missing", err.Error())
				if cErr != nil {
					return nil, fmt.Errorf("%v; cleanup: %w", err, cErr)
				}
				return nil, err
			}
		}
		bindName := req.Name
		if live != nil && live.Name != "" {
			bindName = live.Name
		}
		var berr error
		agentSessionID, berr = LiveWorkerBinding(bindName, tabID, paneID, agentSessionID)
		if berr != nil {
			cErr := closeTab()
			_ = req.Policy.RecordFatal(EventPolicyBlock, "worker_binding_failed", berr.Error())
			if cErr != nil {
				return nil, fmt.Errorf("%v; cleanup: %w", berr, cErr)
			}
			return nil, berr
		}
	} else if req.SkipContainment {
		var berr error
		agentSessionID, berr = LiveWorkerBinding(req.Name, tabID, paneID, "")
		if berr != nil {
			cErr := closeTab()
			if cErr != nil {
				return nil, fmt.Errorf("%v; cleanup: %w", berr, cErr)
			}
			return nil, berr
		}
	} else {
		cErr := closeTab()
		err := fmt.Errorf("%w: SessionResolver required", ErrSessionUnattested)
		_ = req.Policy.RecordFatal(EventPolicyBlock, "session_resolver_required", err.Error())
		if cErr != nil {
			return nil, fmt.Errorf("%v; cleanup: %w", err, cErr)
		}
		return nil, err
	}
	if err := RefuseProvisionalWorkerSession(agentSessionID); err != nil {
		cErr := closeTab()
		_ = req.Policy.RecordFatal(EventPolicyBlock, "provisional_session", agentSessionID)
		if cErr != nil {
			return nil, fmt.Errorf("%v; cleanup: %w", err, cErr)
		}
		return nil, err
	}
	// Causal start barrier: publish live AgentSessionID into the containment
	// env file so the wrapper (waiting on HERD_SEAL_WAIT) can require exact
	// worker binding before sandbox-exec. Seal is written by the coordinator
	// after this returns (bindLaunchControl).
	if !req.SkipContainment {
		envFile := filepath.Join(req.Grant.CWD, ".herd", "contain", "env.list")
		if err := UpsertEnvFileKeys(envFile, map[string]string{
			"HERD_EXPECTED_WORKER": agentSessionID,
		}); err != nil {
			// Non-fatal only when seal wait is not in use; fail closed if wait barrier set.
			if req.Ambient != nil && req.Ambient["HERD_SEAL_WAIT"] == "1" {
				cErr := closeTab()
				if cErr != nil {
					return nil, fmt.Errorf("publish live worker to env: %w; cleanup: %v", err, cErr)
				}
				return nil, fmt.Errorf("publish live worker to env: %w", err)
			}
		}
	}

	if err := req.Policy.RecordFatal(EventKind("launch"), "sandboxed_agent_started", req.Name); err != nil {
		cErr := closeTab()
		if cErr != nil {
			return nil, fmt.Errorf("%v; cleanup: %w", err, cErr)
		}
		return nil, err
	}

	digest, derr := ComputePolicyDigest(req.Policy)
	if derr != nil {
		cErr := closeTab()
		if cErr != nil {
			return nil, fmt.Errorf("%v; cleanup: %w", derr, cErr)
		}
		return nil, derr
	}
	gen, gerr := NewGeneration()
	if gerr != nil {
		cErr := closeTab()
		if cErr != nil {
			return nil, fmt.Errorf("%v; cleanup: %w", gerr, cErr)
		}
		return nil, gerr
	}
	cwdRel := RelIdentity(req.Grant.CWD, req.Policy.SharedCheckout)
	att := SessionAttestation{
		Generation:      gen,
		AgentSessionID:  agentSessionID,
		TaskRef:         req.TaskRef,
		LeaseGeneration: req.LeaseGeneration,
		PolicyDigest:    digest,
		AgentName:       req.Name,
		Kind:            req.Kind,
		Role:            req.Grant.Role,
		Network:         req.Grant.Network,
		CWDRel:          cwdRel,
		Containment:     partial.Containment,
		TabID:           tabID,
		PaneID:          paneID,
		LaunchedAt:      time.Now().UTC(),
	}
	if err := WriteSessionAttestation(req.Policy.SharedCheckout, att); err != nil {
		_ = req.Policy.RecordFatal(EventPolicyBlock, "attestation_write_failed", err.Error())
		cErr := closeTab()
		if cErr != nil {
			return nil, fmt.Errorf("session attestation: %w; cleanup: %v", err, cErr)
		}
		return nil, fmt.Errorf("session attestation: %w", err)
	}
	partial.Generation = gen
	partial.PolicyDigest = digest
	partial.AgentSessionID = agentSessionID
	if brokerLaunch != nil {
		// Mandatory provisional→tab rebind + readback for durable brokers.
		// Transactional: old path retained until new path verified; cleanup tracks both.
		if brokerLaunch.StatePath != "" {
			oldPath := brokerLaunch.StatePath
			newPath := BrokerStatePath(req.Policy.SharedCheckout, tabID)
			brokerLaunch.AltPaths = append(brokerLaunch.AltPaths, oldPath, newPath)
			if err := RebindBrokerState(oldPath, newPath, tabID, agentSessionID); err != nil {
				_ = brokerLaunch.Close() // stop via any known path; no orphan
				brokerLaunch = nil
				_ = closeTab()
				_ = req.Policy.RecordFatal(EventPolicyBlock, "broker_rebind_failed", err.Error())
				return nil, fmt.Errorf("broker provisional rebind: %w", err)
			}
			brokerLaunch.StatePath = newPath
			brokerLaunch.TabKey = tabID
			brokerLaunch.AltPaths = []string{newPath}
			back, rerr := ReadBrokerState(newPath)
			if rerr != nil || back.TabID != tabID {
				_ = brokerLaunch.Close()
				brokerLaunch = nil
				_ = closeTab()
				return nil, fmt.Errorf("broker rebind readback failed: %v", rerr)
			}
		}
		if brokerLaunch.Inline != nil {
			if err := RegisterTabBroker(tabID, brokerLaunch.Inline); err != nil {
				_ = closeTab()
				return nil, err
			}
		}
		// Ownership: tab close / CloseTabBrokerAt must stop durable process.
		// Do not Close() brokerLaunch on success.
		brokerLaunch = nil
	}
	_ = req.Policy.RecordFatal(EventKind("launch"), "session_attested", gen)
	return partial, nil
}

func memoryFrom(s EventSink) *MemorySink {
	switch t := s.(type) {
	case *MemorySink:
		return t
	case *MultiSink:
		for _, sub := range t.Sinks {
			if m, ok := sub.(*MemorySink); ok {
				return m
			}
		}
	}
	return &MemorySink{}
}

func prependPATH(env []string, prefix string) []string {
	out := make([]string, 0, len(env)+1)
	found := false
	for _, e := range env {
		if strings.HasPrefix(e, "PATH=") {
			out = append(out, "PATH="+prefix+string(os.PathListSeparator)+strings.TrimPrefix(e, "PATH="))
			found = true
			continue
		}
		out = append(out, e)
	}
	if !found {
		out = append(out, "PATH="+prefix)
	}
	return out
}
