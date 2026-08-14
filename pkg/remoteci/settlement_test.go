package remoteci

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/recovery"
)

func TestSettleRejectsStaleCandidateSHA(t *testing.T) {
	watch := Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("a", 40),
		PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}
	settlement := Settlement{
		Version: Version1,
		Binding: Binding{
			Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("b", 40),
			PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
		},
		State: StatePassed,
	}
	if err := Settle(watch, settlement); !errors.Is(err, ErrStale) {
		t.Fatalf("Settle stale candidate error = %v, want ErrStale", err)
	}
}

func TestStoreDeduplicatesCandidateAndPolicyAndRedactsDiagnostics(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("c", 40), PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"}}
	if _, created, err := store.Register(binding); err != nil || !created {
		t.Fatalf("Register first = created=%v err=%v", created, err)
	}
	if _, created, err := store.Register(binding); err != nil || created {
		t.Fatalf("Register duplicate = created=%v err=%v", created, err)
	}
	if _, _, err := store.Register(Binding{Repository: binding.Repository, CandidateSHA: binding.CandidateSHA, PolicyRevision: binding.PolicyRevision, Attempt: 2, RequiredChecks: []string{"gate"}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Register changed attempt = %v, want ErrInvalid", err)
	}
	written, err := store.PersistTerminal(Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "Authorization: secret-token\n" + strings.Repeat("x", 2048)})
	if err != nil || !written {
		t.Fatalf("PersistTerminal = written=%v err=%v", written, err)
	}
	if written, err := store.PersistTerminal(Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "Authorization: secret-token\n" + strings.Repeat("x", 2048)}); err != nil || written {
		t.Fatalf("duplicate terminal = written=%v err=%v", written, err)
	}
	records, err := store.readLocked()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || strings.Contains(records[0].Diagnostic, "secret-token") || len(records[0].Diagnostic) > maxDiagnosticBytes+len("…") {
		t.Fatalf("stored redacted bounded record = %+v", records)
	}
}

func TestGitHubActionsFailsClosedAndRequiresExactCandidate(t *testing.T) {
	binding := Binding{Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("d", 40), PolicyRevision: "p", Attempt: 1, RequiredChecks: []string{"gate"}}
	for name, tc := range map[string]struct {
		output string
		err    error
		want   error
	}{
		"unavailable": {err: errors.New("no auth"), want: ErrUnavailable},
		"no checks":   {output: "[]", want: ErrNoChecks},
		"pending":     {output: `[{"name":"gate","headSha":"` + binding.CandidateSHA + `","status":"in_progress","conclusion":""}]`, want: ErrPending},
		"stale SHA":   {output: `[{"name":"gate","headSha":"` + strings.Repeat("e", 40) + `","status":"completed","conclusion":"success"}]`, want: ErrStale},
	} {
		t.Run(name, func(t *testing.T) {
			g := GitHubActions{Execute: func(context.Context, string, ...string) ([]byte, error) { return []byte(tc.output), tc.err }}
			_, err := g.Watch(context.Background(), binding)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Watch error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestGitHubActionsRequiresEveryDeclaredCheckToPass(t *testing.T) {
	binding := Binding{Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("9", 40), PolicyRevision: "p", Attempt: 1, RequiredChecks: []string{"build", "lint"}}
	for name, tc := range map[string]struct {
		output string
		want   error
	}{
		"missing required": {output: `[{"name":"build","headSha":"` + binding.CandidateSHA + `","status":"completed","conclusion":"success"}]`, want: ErrNoChecks},
		"failed required":  {output: `[{"name":"build","headSha":"` + binding.CandidateSHA + `","status":"completed","conclusion":"success"},{"name":"lint","headSha":"` + binding.CandidateSHA + `","status":"completed","conclusion":"failure"}]`},
	} {
		t.Run(name, func(t *testing.T) {
			g := GitHubActions{Execute: func(context.Context, string, ...string) ([]byte, error) { return []byte(tc.output), nil }}
			settlement, err := g.Watch(context.Background(), binding)
			if tc.want != nil {
				if !errors.Is(err, tc.want) {
					t.Fatalf("Watch error = %v, want %v", err, tc.want)
				}
				return
			}
			if err != nil || settlement.State != StateFailed {
				t.Fatalf("Watch = %+v, %v; want terminal failure", settlement, err)
			}
		})
	}
}

func TestTerminalFailureRoutesToRecoveryOnce(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "settlements.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := recovery.Open(filepath.Join(t.TempDir(), "recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	binding := Binding{Repository: "github.com/Kampe/Herdforge", CandidateSHA: strings.Repeat("f", 40), PolicyRevision: "p", Attempt: 1, RequiredChecks: []string{"gate"}}
	if _, _, err := store.Register(binding); err != nil {
		t.Fatal(err)
	}
	settlement := Settlement{Version: Version1, Binding: binding, State: StateFailed, Diagnostic: "failed"}
	router := RecoveryRouter{Store: r, Run: "run", Task: "task", Actor: "remote-ci", Revision: 1, Graph: "graph"}
	if err := PersistAndRouteTerminal(context.Background(), store, settlement, router); err != nil {
		t.Fatal(err)
	}
	if err := PersistAndRouteTerminal(context.Background(), store, settlement, router); err != nil {
		t.Fatal(err)
	}
	if decisions := r.Decisions("run", "task"); len(decisions) != 1 {
		t.Fatalf("recovery decisions = %+v", decisions)
	}
}

func TestSettlementRequiresExactBinding(t *testing.T) {
	settlement := Settlement{Version: Version1, Binding: Binding{
		Repository: "github.com/Kampe/Herdforge", CandidateSHA: "refs/heads/main", PolicyRevision: "policy-v1", Attempt: 1, RequiredChecks: []string{"gate"},
	}, State: StatePassed}
	if err := settlement.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate error = %v, want ErrInvalid", err)
	}
}
