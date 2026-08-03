package launch

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/router"
)

func good() Request {
	d, err := router.NewRouter(nil, nil).Decide(router.LaunchRequest{Role: router.RoleWorker, Shape: Implementation, RequestedProvider: WorkerProvider, RequestedModel: WorkerModel, RequestedEffort: WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(WorkerProvider, WorkerModel): true}})
	if err != nil {
		panic(err)
	}
	return Request{Decision: d}
}

func TestValidateWorkerDecisionDoesNotPreAccept(t *testing.T) {
	s := &MemorySink{}
	if err := Validate(good(), s); err != nil {
		t.Fatal(err)
	}
	if len(s.Receipts) != 0 {
		t.Fatalf("validation must not imply process launch: %+v", s.Receipts)
	}
	if err := RecordStarted(good(), s); err != nil {
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
			req := good()
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
	r := good()
	r.Decision.Provider = " CODEX "
	r.Decision.Model = "openai/GPT-5.6-LUNA"
	if err := Validate(r, &MemorySink{}); err != nil {
		t.Fatal(err)
	}
}

func TestRecoveryDecisionUsesSameWorkerBoundary(t *testing.T) {
	d, err := router.NewRouter(nil, nil).Decide(router.LaunchRequest{Role: router.RoleRecovery, Shape: Implementation, RequestedProvider: WorkerProvider, RequestedModel: WorkerModel, RequestedEffort: WorkerEffort, ProbeResults: map[string]bool{router.ProbeKey(WorkerProvider, WorkerModel): true}})
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
	a, b := good(), good()
	b.Decision.Argv = append([]string(nil), b.Decision.Argv...)
	b.Decision.Argv[3] = "-x"
	if DecisionDigest(a.Decision) == DecisionDigest(b.Decision) {
		t.Fatal("decision digest must change when routed argv changes")
	}
}

func TestHandBuiltApprovedTupleFailsClosed(t *testing.T) {
	r := Request{Decision: &router.LaunchDecision{Role: router.RoleWorker, Shape: Implementation, Provider: WorkerProvider, Model: WorkerModel, Effort: WorkerEffort, Argv: []string{"codex", "--model", WorkerModel, "-c", "model_reasoning_effort=medium", "-a", "never"}}}
	if err := Validate(r, &MemorySink{}); err == nil {
		t.Fatal("hand-built approved tuple must require router proof")
	}
}

func TestEditedRouterDecisionFailsProof(t *testing.T) {
	r := good()
	r.Decision.Effort = "high"
	if err := Validate(r, &MemorySink{}); err == nil {
		t.Fatal("edited routed decision must fail proof verification")
	}
}
