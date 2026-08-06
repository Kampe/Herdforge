package launch

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

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
		CLIPresent: func(cli string) bool { return cli == testWorkerProvider },
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
