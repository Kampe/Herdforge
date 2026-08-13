package dispatch

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/residual"
)

func TestBuildTaskPacketProjectsRevisionBoundResidual(t *testing.T) {
	r, err := residual.New(residual.KindDeferredFunction, residual.SeverityMedium, "defer migration", "id-237", "FAC-237", "rev-237", "receipt:237", false)
	if err != nil {
		t.Fatal(err)
	}
	task := &provider.Task{ID: "id-237", Ref: "FAC-237", Residuals: []residual.Record{r}}
	packet := buildTaskPacket(task, "task/fac-237", ".herd/prompts/worker.md", "memory", "p", nil, config.Verification{TestCommand: "go test ./pkg/dispatch"}, ReplyTarget{})
	section, err := residual.PacketSection(task.Residuals)
	if err != nil {
		t.Fatal(err)
	}
	packet += "\nResidual work (does not waive acceptance criteria):\n" + section
	if !strings.Contains(packet, "RESIDUAL id="+r.ID) || !strings.Contains(packet, "revision=rev-237") {
		t.Fatalf("packet omitted residual binding: %q", packet)
	}
}
