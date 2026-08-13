package mergeadmit

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/residual"
)

func TestAdmitRefusesUnlinkedResidualBeforeAnyMergeAuthority(t *testing.T) {
	r, err := residual.New(residual.KindDeferredFunction, residual.SeverityLow, "follow up", "task-237", "FAC-237", "revision-237", "receipt:237", false)
	if err != nil {
		t.Fatal(err)
	}
	d, err := (&Gate{}).Admit(Request{ProviderRevision: "revision-237", Residuals: []residual.Record{r}})
	if err == nil || d == nil || d.Code != CodeResidual || !strings.Contains(d.Reason, "follow-up linkage") {
		t.Fatalf("unlinked residual was not fail-closed: decision=%+v err=%v", d, err)
	}
}
