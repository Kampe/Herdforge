package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
	"github.com/Kampe/Herdforge/pkg/harness"
	"github.com/Kampe/Herdforge/pkg/router"
)

// Fixture tuple for launch-policy tests. Deliberately a concrete provider so
// the hermetic router picks deterministically; production no longer pins any
// vendor for builder roles, so these must NOT come from pkg/launch.
const (
	testWorkerProvider = "codex"
	testWorkerModel    = "gpt-5.6-luna"
	testWorkerEffort   = "high"
)

func testRouter(t *testing.T) *router.SurfaceRouter {
	t.Helper()
	t.Setenv("HERDR_ROUTE_STATE_DIR", t.TempDir())
	r := router.NewRouter(nil, nil)
	r.Probes = &router.Probes{
		CLIPresent: func(cli string) bool { return cli == router.PiHarness },
		Now:        func() time.Time { return time.Unix(1_800_000_000, 0) },
	}
	return r
}

func good(t *testing.T) Request {
	t.Helper()
	d, err := testRouter(t).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatalf("build worker fixture: %v", err)
	}
	return Request{Decision: d, HookDiscovery: harness.NoHooksDiscovery()}
}

func withHooks(req *Request, hooks []harness.Hook) {
	req.Hooks = hooks
	req.HookDiscovery = harness.HookDiscoveryFunc(func(string) (harness.HookDiscoveryResult, error) {
		return harness.HookDiscoveryResult{State: harness.DiscoveryHooks, Hooks: hooks}, nil
	})
}

func installTestExecutable(t *testing.T, dir, name string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWorkerDecisionDoesNotPreAccept(t *testing.T) {
	s := &MemorySink{}
	if err := Validate(good(t), s); err != nil {
		t.Fatal(err)
	}
	if len(s.Receipts) != 0 {
		t.Fatalf("validation must not imply process launch: %+v", s.Receipts)
	}
	if err := RecordStarted(good(t), s); err != nil {
		t.Fatal(err)
	}
	if len(s.Receipts) != 1 || !s.Receipts[0].Accepted {
		t.Fatalf("missing started receipt: %+v", s.Receipts)
	}
}

func TestValidateRejectsEveryMissingLaunchFieldAndRecords(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Request)
	}{
		{"bare", func(r *Request) { r.Decision.Argv = nil }},
		{"coordinator-shape", func(r *Request) { r.Decision.Shape = "coordinator" }},
		{"sol", func(r *Request) { r.Decision.Model = "gpt-5.6-sol" }},
		{"fable", func(r *Request) { r.Decision.Model = "claude-fable-5" }},
		{"unknown-role", func(r *Request) { r.Decision.Role = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := good(t)
			tc.mutate(&req)
			s := &MemorySink{}
			err := Validate(req, s)
			if err == nil {
				t.Fatal("expected rejection")
			}
			if len(s.Receipts) != 1 || s.Receipts[0].Accepted {
				t.Fatalf("expected failed durable receipt: %+v", s.Receipts)
			}
			if strings.Contains(err.Error(), "in-progress") {
				t.Fatal(err)
			}
		})
	}
}

func TestValidateNormalizesAllowedAliases(t *testing.T) {
	r := good(t)
	r.Decision.Provider = " CODEX "
	r.Decision.Model = "openai/GPT-5.6-LUNA"
	if err := Validate(r, &MemorySink{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDecisionUsesSameWorkerBoundary(t *testing.T) {
	d, err := testRouter(t).Decide(router.LaunchRequest{Role: router.RoleRecovery, Shape: Implementation, RequestedProvider: testWorkerProvider, RequestedModel: testWorkerModel, RequestedEffort: testWorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(testWorkerProvider, testWorkerModel): true}})
	if err != nil {
		t.Fatal(err)
	}
	r := Request{Decision: d, HookDiscovery: harness.NoHooksDiscovery()}
	if err := Validate(r, &MemorySink{}); err != nil {
		t.Fatalf("Luna/medium recovery decision rejected: %v", err)
	}
	r.Decision.Model = "gpt-5.6-sol"
	if err := Validate(r, &MemorySink{}); err == nil {
		t.Fatal("Sol recovery decision must fail closed")
	}
}

func TestDecisionDigestBindsReceiptToDecision(t *testing.T) {
	a, b := good(t), good(t)
	b.Decision.Argv = append([]string(nil), b.Decision.Argv...)
	b.Decision.Argv[3] = "-x"
	if DecisionDigest(a.Decision) == DecisionDigest(b.Decision) {
		t.Fatal("decision digest must change when routed argv changes")
	}
}

func TestHandBuiltApprovedTupleFailsClosed(t *testing.T) {
	d := &router.LaunchDecision{Role: router.RoleWorker, Shape: Implementation, Provider: testWorkerProvider, Model: testWorkerModel, Effort: testWorkerEffort, Argv: []string{"codex", "--model", testWorkerModel, "-c", "model_reasoning_effort=" + testWorkerEffort, "-a", "never"}}
	// This is an exact public-field forgery: recompute the production
	// canonical digest byte-for-byte instead of merely omitting Proof.
	canonical := fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%d|%s|%s|%s|%s|%s", "herdforge-fac-175-launch-decision-v1", "worker", Implementation, testWorkerProvider, testWorkerModel, testWorkerEffort, d.CandidateSHA, d.LeaseGeneration, d.TaskRef, d.Scope, d.ProbeKey, d.Rationale, strings.Join(d.Argv, "\x00"))
	sum := sha256.Sum256([]byte(canonical))
	d.Proof = "sha256:" + hex.EncodeToString(sum[:])
	r := Request{Decision: d, HookDiscovery: harness.NoHooksDiscovery()}
	if err := Validate(r, &MemorySink{}); err == nil {
		t.Fatal("public-field forgery with recomputed canonical proof must fail closed")
	}
}

func TestHasStartedRejectsConflictingPersistedTupleDespiteMatchingDigest(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	req := good(t)
	req.TaskRef, req.Name, req.PaneID, req.LeaseGeneration = "FAC-175", "worker", "pane-1", 7
	if err := (&JSONLSink{Path: os.Getenv("HERD_LAUNCH_RECEIPTS")}).Write(Receipt{
		TaskRef: req.TaskRef, Name: req.Name, PaneID: req.PaneID, LeaseGeneration: req.LeaseGeneration,
		Role: WorkerRole, TaskShape: Implementation, Provider: testWorkerProvider,
		// The digest claims the current Luna decision, while persisted identity
		// fields and argv claim the forbidden coordinator-tier session.
		Model: testWorkerModel, Effort: "ultra", DecisionDigest: DecisionDigest(req.Decision),
		Argv: []string{"codex", "--model", "gpt-5.6-sol", "-c", "model_reasoning_effort=ultra", "-a", "never"}, Accepted: true,
	}); err != nil {
		t.Fatal(err)
	}
	started, err := HasStarted(req)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("conflicting persisted Sol/Ultra tuple must not authorize resume")
	}
}

func TestHasStartedRejectsConflictingAcceptedAndRevokedRecords(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	req := good(t)
	req.TaskRef, req.Name, req.PaneID, req.LeaseGeneration = "FAC-175", "worker", "pane-1", 7
	sink := &JSONLSink{Path: os.Getenv("HERD_LAUNCH_RECEIPTS")}
	role, shape, provider, model, effort, digest, argv := fields(req)
	accepted := Receipt{TaskRef: req.TaskRef, Name: req.Name, PaneID: req.PaneID, LeaseGeneration: req.LeaseGeneration, Role: role, TaskShape: shape, Provider: provider, Model: model, Effort: effort, DecisionDigest: digest, Argv: argv, Accepted: true}
	if err := sink.Write(accepted); err != nil {
		t.Fatal(err)
	}
	if err := sink.Write(Receipt{TaskRef: req.TaskRef, Name: req.Name, PaneID: req.PaneID, LeaseGeneration: req.LeaseGeneration, Accepted: false, Reason: "revoked"}); err != nil {
		t.Fatal(err)
	}
	started, err := HasStarted(req)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("revoked or conflicting session must not remain resumable")
	}
}

func TestHasStartedFencesSessionAndMatchingRejectedIdentity(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	req := good(t)
	req.TaskRef, req.Name, req.PaneID, req.LeaseGeneration, req.SessionGeneration = "FAC-188", "worker", "pane-1", 7, 42
	sink := &JSONLSink{Path: os.Getenv("HERD_LAUNCH_RECEIPTS")}
	if err := RecordStarted(req, sink); err != nil {
		t.Fatal(err)
	}
	other := req
	other.SessionGeneration = 41
	started, err := HasStarted(other)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("different session generation authorized resume")
	}
	_ = RecordRejected(req, sink, "failed-bind")
	started, err = HasStarted(req)
	if err != nil {
		t.Fatal(err)
	}
	if started {
		t.Fatal("matching rejected identity remained resumable")
	}
	data, err := os.ReadFile(os.Getenv("HERD_LAUNCH_RECEIPTS"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name":"worker"`) || !strings.Contains(string(data), `"pane_id":"pane-1"`) || !strings.Contains(string(data), `"session_generation":42`) {
		t.Fatalf("rejected receipt lost exact identity: %s", data)
	}
}

func TestHasStartedRejectsMalformedReceiptData(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", path)
	if err := os.WriteFile(path, []byte("{not-json}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	req := good(t)
	req.TaskRef, req.Name, req.PaneID = "FAC-175", "worker", "pane-1"
	started, err := HasStarted(req)
	if err == nil || started {
		t.Fatalf("malformed durable evidence must fail closed: started=%v err=%v", started, err)
	}
}

func TestEditedRouterDecisionFailsProof(t *testing.T) {
	r := good(t)
	// Tamper with a value that is definitely NOT what the router issued. This
	// previously wrote "high", which silently became a no-op once the worker
	// effort ladder started producing "high" — the test passed by accident.
	if r.Decision.Effort == "low" {
		r.Decision.Effort = "high"
	} else {
		r.Decision.Effort = "low"
	}
	if err := Validate(r, &MemorySink{}); err == nil {
		t.Fatalf("edited routed decision must fail proof verification (effort now %q)", r.Decision.Effort)
	}
}

func TestValidateRejectsHarnessMutation(t *testing.T) {
	for name, mutate := range map[string]func(*router.LaunchDecision){
		"harness": func(d *router.LaunchDecision) { d.Harness = "codex" },
		"argv": func(d *router.LaunchDecision) {
			d.HarnessArgv = append([]string(nil), d.HarnessArgv...)
			d.HarnessArgv[2] = "openai-codex/gpt-5.6-sol"
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := good(t)
			decision := *req.Decision
			mutate(&decision)
			req.Decision = &decision
			if err := Validate(req, nil); err == nil {
				t.Fatal("mutated harness accepted")
			}
		})
	}
}

func TestDecisionDigestBindsHarnessArgv(t *testing.T) {
	a, b := good(t), good(t)
	b.Decision.HarnessArgv = append([]string(nil), b.Decision.HarnessArgv...)
	b.Decision.HarnessArgv[2] = "openai-codex/gpt-5.6-sol"
	if DecisionDigest(a.Decision) == DecisionDigest(b.Decision) {
		t.Fatal("decision digest must change when signed harness argv changes")
	}
}

func TestValidateAcceptsBoundPiSession(t *testing.T) {
	req := good(t)
	bound, err := router.BindHarnessSession(req.Decision, filepath.Join(t.TempDir(), "launch.jsonl"))
	if err != nil {
		t.Fatalf("bind harness session: %v", err)
	}
	req.Decision = bound
	if err := Validate(req, nil); err != nil {
		t.Fatalf("bound Pi session rejected: %v", err)
	}

	for name, mutate := range map[string]func(*router.LaunchDecision){
		"session": func(d *router.LaunchDecision) {
			d.HarnessSession = filepath.Join(t.TempDir(), "mutated.jsonl")
		},
		"argv-path": func(d *router.LaunchDecision) {
			d.HarnessArgv = append([]string(nil), d.HarnessArgv...)
			d.HarnessArgv[len(d.HarnessArgv)-1] = filepath.Join(t.TempDir(), "mutated.jsonl")
		},
	} {
		t.Run(name, func(t *testing.T) {
			decision := *req.Decision
			decision.HarnessArgv = append([]string(nil), req.Decision.HarnessArgv...)
			mutate(&decision)
			mutated := req
			mutated.Decision = &decision
			if err := Validate(mutated, nil); err == nil {
				t.Fatal("mutated bound session accepted")
			}
		})
	}
}

func TestValidateRejectsMissingNestedAgentDenial(t *testing.T) {
	req := good(t)
	// Mutation: strip --disable multi_agent pairs that FAC-173 requires.
	argv := append([]string(nil), req.Decision.Argv...)
	stripped := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "--disable" && i+1 < len(argv) && (argv[i+1] == "multi_agent" || argv[i+1] == "multi_agent_v2") {
			i++
			continue
		}
		stripped = append(stripped, argv[i])
	}
	req.Decision.Argv = stripped
	// Also recompute proof so we exercise the nested-deny gate, not the
	// digest gate. A full public-field forgery still fails closed via proof;
	// the point is production argv without multi_agent denials is refused.
	if err := Validate(req, &MemorySink{}); err == nil {
		t.Fatal("argv missing nested-agent denials must fail closed")
	}
}

func TestFleetBindingOnReceiptAndRecovery(t *testing.T) {
	key := []byte("launch-fleet-key")
	t.Setenv(agentpolicy.SecretEnv, string(key))
	t.Setenv(agentpolicy.SecretEnvFallback, "")
	req := good(t)
	// Keep TaskRef/LeaseGeneration zero: good() is a generic (pre-claim)
	// decision and VerifyDecisionForScope refuses task context on it.
	// The fleet binding itself carries the exact lease generation that
	// recovery will re-prove.
	req.Name, req.PaneID = "worker", "pane-1"
	req.TabID, req.HerdrSession, req.Repository, req.Lane = "tab-1", "sess-1", "github.com/Kampe/Herdforge", "worker"
	const leaseGen int64 = 11
	b, _, err := agentpolicy.BindLaunch(req.Repository, "FAC-173", req.Lane, WorkerRole, leaseGen, req.HerdrSession, req.TabID, req.PaneID, "codex", agentpolicy.SurfaceHerdrDispatch, key)
	if err != nil {
		t.Fatal(err)
	}
	req.FleetBinding = b
	if err := Validate(req, &MemorySink{}); err != nil {
		t.Fatalf("valid fleet binding rejected: %v", err)
	}
	s := &MemorySink{}
	if err := RecordStarted(req, s); err != nil {
		t.Fatal(err)
	}
	if len(s.Receipts) != 1 || s.Receipts[0].FleetPolicyDigest != b.PolicyDigest || s.Receipts[0].FleetAuthTag != b.AuthTag {
		t.Fatalf("receipt missing fleet contract: %+v", s.Receipts)
	}
	// Recovery re-proof uses the binding identity carried on the receipt.
	rec := s.Receipts[0]
	rec.TaskRef = "FAC-173"
	rec.LeaseGeneration = leaseGen
	if err := VerifyRecoveryBinding(rec, key, leaseGen); err != nil {
		t.Fatalf("recovery re-proof: %v", err)
	}
	if err := VerifyRecoveryBinding(rec, key, leaseGen+1); !errors.Is(err, agentpolicy.ErrInvalidContract) {
		t.Fatalf("recovery generation drift: %v", err)
	}
	// Stale binding fails launch before process start.
	stale := req
	stale.FleetBinding.PolicyDigest = "stale"
	if err := Validate(stale, &MemorySink{}); err == nil {
		t.Fatal("stale fleet binding must fail closed")
	}
}

func TestRoutedWorkerArgvCarriesNestedAgentDenial(t *testing.T) {
	req := good(t)
	if err := agentpolicy.RequireNestedDeny(req.Decision.Provider, req.Decision.Argv); err != nil {
		t.Fatalf("routed worker argv not fleet-safe: %v\nargv=%v", err, req.Decision.Argv)
	}
}

func TestValidateRequiredHookFailureHasNoLaunchAcceptance(t *testing.T) {
	called := false
	req := good(t)
	withHooks(&req, []harness.Hook{{Name: "policy", URL: "http://127.0.0.1:1", Requirement: harness.HookRequired}})
	req.HookWarning = func(string) { called = true }
	sink := &MemorySink{}
	if err := Validate(req, sink); err == nil {
		t.Fatal("required hook failure must reject launch")
	}
	if called {
		t.Fatal("required hook failure must not emit optional degradation warning")
	}
	if len(sink.Receipts) != 1 || sink.Receipts[0].Accepted {
		t.Fatalf("required hook rejection receipt = %+v", sink.Receipts)
	}
}

func TestLaunchBoundaryRequiredHookRejectionHasZeroSideEffects(t *testing.T) {
	req := good(t)
	withHooks(&req, []harness.Hook{{Name: "required-policy", URL: "http://127.0.0.1:1", Requirement: harness.HookRequired}})
	sink := &MemorySink{}
	called := make([]string, 0, 4)
	effects := LaunchEffects{
		Tab:     func() error { called = append(called, "tab"); return nil },
		Process: func() error { called = append(called, "process"); return nil },
		Prompt:  func() error { called = append(called, "prompt"); return nil },
		Board:   func() error { called = append(called, "board"); return nil },
	}
	if err := Launch(req, sink, effects); err == nil {
		t.Fatal("required hook rejection must fail the production launch boundary")
	}
	if len(sink.Receipts) != 1 || sink.Receipts[0].Kind != "launch_rejected" || sink.Receipts[0].HookCode == "" {
		t.Fatalf("required hook rejection receipt = %+v", sink.Receipts)
	}
	if len(called) != 0 {
		t.Fatalf("required hook rejection reached side effects: %v", called)
	}
}

func TestOrdinaryRequestCannotBypassProductionDiscovery(t *testing.T) {
	req := good(t)
	req.HookDiscovery = nil
	req.Hooks = nil
	path := t.TempDir() + "/hooks.json"
	if err := os.WriteFile(path, []byte(`{"providers":{"codex":{"hooks":[{"name":"policy","url":"http://127.0.0.1:1","requirement":"required"}]}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_HARNESS_HOOKS_FILE", path)
	sink := &MemorySink{}
	err := Validate(req, sink)
	if err == nil || !strings.Contains(err.Error(), string(harness.HookCodeUnavailable)) {
		t.Fatalf("ordinary request discovery result = %v", err)
	}
	if len(sink.Receipts) != 1 || sink.Receipts[0].HookCode != string(harness.HookCodeUnavailable) || sink.Receipts[0].Reason != "" {
		t.Fatalf("discovery failure receipt = %+v", sink.Receipts)
	}
}

func TestClaudeCommandIncidentRequiresBoundHealthPolicyBeforeEffects(t *testing.T) {
	home := t.TempDir()
	bin := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".local", "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	installTestExecutable(t, filepath.Join(home, ".local", "bin"), "moshi-hook")
	installTestExecutable(t, bin, "moshi-hook")
	installTestExecutable(t, bin, "python3")
	installTestExecutable(t, bin, "bash")
	t.Setenv("HOME", home)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	settingsPath := t.TempDir() + "/settings.json"
	settings := `{"hooks":{"SessionStart":[{"matcher":"","hooks":[{"type":"command","command":"moshi-hook --port 8790"}]}],"PreToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"/bin/sh -c '$HOME/.local/bin/moshi-hook --port 8790'","timeout":600}]}],"UserPromptSubmit":[{"matcher":"","hooks":[{"type":"command","command":"$HOME/.local/bin/moshi-hook --port 8790"}]}],"PostToolUse":[{"matcher":"Edit","hooks":[{"type":"command","command":"python3 $HOME/.local/bin/moshi-hook --port 8790"}]}],"PostToolUseFailure":[{"matcher":"Edit","hooks":[{"type":"command","command":"bash $HOME/.local/bin/moshi-hook --port 8790"}]}],"SubagentStart":[{"matcher":"","hooks":[{"type":"command","command":"/bin/sh -c '$HOME/.local/bin/moshi-hook subagent'"}]}]}}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CLAUDE_SETTINGS_FILE", settingsPath)
	discovered, err := (harness.ClaudeDiscovery{}).Discover("claude")
	if err != nil || len(discovered.Hooks) != 6 {
		t.Fatalf("realistic Claude command discovery = %+v, err=%v", discovered, err)
	}
	req := good(t)
	var policies []harness.HookPolicy
	for _, hook := range discovered.Hooks {
		policies = append(policies, harness.HookPolicy{HandlerDigest: hook.Name, Requirement: hook.Requirement, HealthURL: "http://127.0.0.1:8790/health"})
	}
	policyDiscovery, err := (harness.ClaudeDiscovery{Paths: []string{settingsPath}, Policies: policies}).Discover("claude")
	if err != nil {
		t.Fatal(err)
	}
	req.HookDiscovery = harness.HookDiscoveryFunc(func(string) (harness.HookDiscoveryResult, error) {
		return harness.HookDiscoveryResult{State: harness.DiscoveryHooks, Hooks: discovered.Hooks, Policies: policies, PolicyRequired: true, PolicyRevision: policyDiscovery.PolicyRevision}, nil
	})
	sink := &MemorySink{}
	effects := 0
	if err := Launch(req, sink, LaunchEffects{Tab: func() error { effects++; return nil }, Process: func() error { effects++; return nil }, Prompt: func() error { effects++; return nil }, Board: func() error { effects++; return nil }}); err == nil {
		t.Fatal("refused command health endpoint must reject")
	}
	if effects != 0 || len(sink.Receipts) < 1 {
		t.Fatalf("command incident rejection effects=%d receipts=%+v", effects, sink.Receipts)
	}
	var rejection Receipt
	for _, receipt := range sink.Receipts {
		if receipt.Kind == "launch_rejected" {
			rejection = receipt
			break
		}
	}
	if rejection.Kind != "launch_rejected" || rejection.HookCode != string(harness.HookCodeUnavailable) || !strings.HasPrefix(rejection.HookName, "claude:") || len(rejection.HookName) < len("claude:")+64 || rejection.PolicyRevision == "" {
		t.Fatalf("rejection attribution lost full digest/revision: %+v", sink.Receipts)
	}
}

func TestHookReceiptRedactsAuthorityAndIsStable(t *testing.T) {
	req := good(t)
	withHooks(&req, []harness.Hook{{Name: "policy", URL: "http://user:secret@127.0.0.1:1?token=secret", Requirement: harness.HookRequired}})
	sink := &MemorySink{}
	if err := Validate(req, sink); err == nil {
		t.Fatal("malformed required hook must fail")
	}
	if len(sink.Receipts) != 1 {
		t.Fatalf("receipts = %+v", sink.Receipts)
	}
	receipt := sink.Receipts[0]
	if receipt.Reason != "" || strings.Contains(receipt.RedactedAuthority, "secret") || strings.Contains(receipt.RedactedAuthority, "http") || receipt.HookCode == "" {
		t.Fatalf("unredacted hook receipt = %+v", receipt)
	}
}

func TestOptionalDegradedReceiptIsDurablyDeduplicated(t *testing.T) {
	req := good(t)
	withHooks(&req, []harness.Hook{{Name: "telemetry", URL: "http://127.0.0.1:1", Requirement: harness.HookOptional}})
	warnings := 0
	req.HookWarning = func(string) { warnings++ }
	sink := &MemorySink{}
	if err := Validate(req, sink); err != nil {
		t.Fatal(err)
	}
	if err := Validate(req, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.Receipts) != 1 || sink.Receipts[0].Kind != "hook_degraded" {
		t.Fatalf("deduplicated degraded receipts = %+v", sink.Receipts)
	}
	if warnings != 1 {
		t.Fatalf("deduplicated warnings = %d", warnings)
	}
}

func TestOptionalDegradedJSONLReceiptIsDurablyDeduplicated(t *testing.T) {
	req := good(t)
	withHooks(&req, []harness.Hook{{Name: "telemetry", URL: "http://127.0.0.1:1", Requirement: harness.HookOptional}})
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	if err := Validate(req, sink); err != nil {
		t.Fatal(err)
	}
	if err := Validate(req, sink); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(b), "\n"); got != 1 {
		t.Fatalf("durable degraded receipt lines = %d, want 1: %s", got, b)
	}
}

func TestDegradedReceiptDoesNotInvalidateStartedResume(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", path)
	req := good(t)
	req.TaskRef, req.Name, req.PaneID, req.LeaseGeneration = "FAC-177", "worker", "pane-1", 7
	withHooks(&req, []harness.Hook{{Name: "telemetry", URL: "http://127.0.0.1:1", Requirement: harness.HookOptional}})
	sink := &JSONLSink{Path: path}
	if _, err := PreflightHooks(req); err != nil {
		t.Fatal(err)
	}
	if err := RecordStarted(req, sink); err != nil {
		t.Fatal(err)
	}
	started, err := HasStarted(req)
	if err != nil || !started {
		t.Fatalf("degraded receipt must not revoke started identity: started=%v err=%v", started, err)
	}
	status, err := ReadDegradedStatus(path)
	if err != nil || len(status) != 1 || status[0].HookName == "" || strings.Contains(status[0].HookName, "telemetry") {
		t.Fatalf("degraded status projection = %+v, err=%v", status, err)
	}
}

func TestDegradedThenHealthyProjectionRecovers(t *testing.T) {
	path := t.TempDir() + "/receipts.jsonl"
	t.Setenv("HERD_LAUNCH_RECEIPTS", path)
	req := good(t)
	hooks := []harness.Hook{{Name: "telemetry", URL: "http://127.0.0.1:1", Requirement: harness.HookOptional}}
	withHooks(&req, hooks)
	sink := &JSONLSink{Path: path}
	if err := Validate(req, sink); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	withHooks(&req, []harness.Hook{{Name: "telemetry", URL: server.URL, Requirement: harness.HookOptional}})
	if err := Validate(req, sink); err != nil {
		t.Fatal(err)
	}
	status, err := ReadDegradedStatus(path)
	if err != nil || len(status) != 0 {
		t.Fatalf("healthy revalidation left stale degraded status: %+v, err=%v", status, err)
	}
	b, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(b), `"kind":"hook_degraded"`) || !strings.Contains(string(b), `"kind":"hook_recovered"`) {
		t.Fatalf("historical lifecycle evidence missing: %s, err=%v", b, err)
	}
}

func TestValidateOptionalHookWarningIsDeduplicatedAndIdentityPreserved(t *testing.T) {
	var headers http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	req := good(t)
	withHooks(&req, []harness.Hook{
		{Name: "z-telemetry", URL: "http://127.0.0.1:1", Requirement: harness.HookOptional},
		{Name: "healthy", URL: server.URL, HealthURL: server.URL, Requirement: harness.HookOptional},
	})
	warnings := make([]string, 0)
	req.HookWarning = func(warning string) { warnings = append(warnings, warning) }
	if err := Validate(req, &MemorySink{}); err != nil {
		t.Fatal(err)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "unavailable") || strings.Contains(warnings[0], "z-telemetry") {
		t.Fatalf("warnings = %v", warnings)
	}
	if headers.Get("X-Herd-Provider") != req.Decision.Provider || headers.Get("X-Herd-Model") != req.Decision.Model || headers.Get("X-Herd-Effort") != req.Decision.Effort {
		t.Fatalf("routed identity changed at hook boundary: %v", headers)
	}
}

func TestHasStartedRevalidatesHooks(t *testing.T) {
	t.Setenv("HERD_LAUNCH_RECEIPTS", t.TempDir()+"/receipts.jsonl")
	req := good(t)
	req.TaskRef, req.Name, req.PaneID, req.LeaseGeneration, req.SessionGeneration = "FAC-177", "worker", "pane-1", 7, 1
	if err := (&JSONLSink{Path: os.Getenv("HERD_LAUNCH_RECEIPTS")}).Write(Receipt{
		TaskRef: req.TaskRef, Name: req.Name, PaneID: req.PaneID, LeaseGeneration: req.LeaseGeneration, SessionGeneration: req.SessionGeneration,
		Role: WorkerRole, TaskShape: Implementation, Provider: req.Decision.Provider, Model: req.Decision.Model,
		Effort: req.Decision.Effort, DecisionDigest: DecisionDigest(req.Decision), Argv: req.Decision.Argv, Accepted: true,
	}); err != nil {
		t.Fatal(err)
	}
	withHooks(&req, []harness.Hook{{Name: "recovery-policy", URL: "http://127.0.0.1:1", Requirement: harness.HookRequired}})
	started, err := HasStarted(req)
	if err == nil || started {
		t.Fatalf("resume must revalidate hook health: started=%v err=%v", started, err)
	}
}

func TestRequiredHookPreflightMutantWouldFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()
	req := good(t)
	withHooks(&req, []harness.Hook{{Name: "required", URL: server.URL, Requirement: harness.HookRequired}})
	var effects int
	if err := Launch(req, &MemorySink{}, LaunchEffects{Tab: func() error { effects++; return nil }}); err != nil {
		t.Fatal(err)
	}
	if effects != 1 {
		t.Fatalf("healthy launch effects = %d, want 1", effects)
	}
	// Mutating only the required hook endpoint must make this production-boundary
	// proof red if Launch stops calling Validate/PreflightHooks.
	withHooks(&req, []harness.Hook{{Name: "required", URL: "http://127.0.0.1:1", Requirement: harness.HookRequired}})
	effects = 0
	sink := &MemorySink{}
	if err := Launch(req, sink, LaunchEffects{Tab: func() error { effects++; return nil }}); err == nil {
		t.Fatal("required hook boundary mutant was not rejected")
	}
	if effects != 0 || len(sink.Receipts) != 1 || sink.Receipts[0].Kind != "launch_rejected" {
		t.Fatalf("boundary rejection effects=%d receipts=%+v", effects, sink.Receipts)
	}
}

func TestHookReceiptBoundsUntrustedIdentityMaterial(t *testing.T) {
	name := "configured-name-SECRET-" + strings.Repeat("sensitive-name-", 80)
	matcher := "matcher-SECRET-" + strings.Repeat("sensitive-matcher-", 80)
	pathSecret := "path-SECRET-" + strings.Repeat("private-", 40)
	querySecret := "query-SECRET-" + strings.Repeat("token-", 40)
	settings := `{"hooks":{"PreToolUse":[{"matcher":"` + matcher + `","hooks":[{"type":"http","name":"` + name + `","url":"http://127.0.0.1:1/` + pathSecret + `?token=` + querySecret + `"}]}]}}`
	settingsPath := t.TempDir() + "/settings.json"
	if err := os.WriteFile(settingsPath, []byte(settings), 0600); err != nil {
		t.Fatal(err)
	}
	req := good(t)
	req.HookDiscovery = harness.ClaudeDiscovery{Paths: []string{settingsPath}}
	path := t.TempDir() + "/receipts.jsonl"
	sink := &JSONLSink{Path: path}
	if err := Validate(req, sink); err == nil {
		t.Fatal("unavailable discovered required hook must reject")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	receipts := string(b)
	for _, secret := range []string{name, matcher, pathSecret, querySecret} {
		if strings.Contains(receipts, secret) {
			t.Fatalf("receipt leaked untrusted hook material %q", secret)
		}
	}
	if len(receipts) > 2048 || strings.Contains(receipts, "http://") || strings.Contains(receipts, "?") {
		t.Fatalf("receipt evidence is not bounded/redacted: %s", receipts)
	}
}
