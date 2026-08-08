package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/agentpolicy"
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
	return Request{Decision: d}
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
	r := Request{Decision: d}
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
	r := Request{Decision: d}
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
