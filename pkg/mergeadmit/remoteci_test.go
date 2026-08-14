package mergeadmit

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/remoteci"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func TestAdmitRequiresPassedRemoteCISettlementBoundToExactCandidate(t *testing.T) {
	dir := t.TempDir()
	ledger := newLedger(t, dir)
	launch(t, ledger, shaCurrent, "reviewer", "anthropic", "builder-session-1")
	verdict(t, ledger, shaCurrent, "reviewer", reviewledger.VerdictPASS)
	gate := okGate(t, ledger, shaBase, shaCurrent)
	gate.RemoteCIRepository = "github.com/Kampe/Herdforge"
	gate.RemoteCIPolicyRevision = "policy-v1"
	req := okRequest(shaBase, shaCurrent)
	mustRefuse(t, gate, req, CodeRemoteCI)

	req.RemoteCI = &remoteci.Settlement{Version: remoteci.Version1, Binding: remoteci.Binding{
		Repository: gate.RemoteCIRepository, CandidateSHA: strings.Repeat("a", 40),
		PolicyRevision: gate.RemoteCIPolicyRevision, Attempt: 1, RequiredChecks: []string{"build"},
	}, State: remoteci.StatePassed}
	mustRefuse(t, gate, req, CodeRemoteCI)

	req.RemoteCI = &remoteci.Settlement{Version: remoteci.Version1, Binding: remoteci.Binding{
		Repository: gate.RemoteCIRepository, CandidateSHA: shaCurrent,
		PolicyRevision: gate.RemoteCIPolicyRevision, Attempt: 1, RequiredChecks: []string{"build"},
	}, State: remoteci.StatePassed}
	mustAdmit(t, gate, req)
}
