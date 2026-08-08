package dispatch

import (
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// TestOwnershipClaimer_UnboundProviderFailsClosed pins FAC-155 at the launch
// lease: lease keys carry board identity, so an unconfigured dispatcher used to
// silently lease under provider "memory" — a board nobody activated. Both rows
// must now be hard errors.
//
// Non-vacuous: restoring `providerType := "memory"` as a default makes both
// subtests reach OpenLeaseOwnership and stop erroring on identity.
func TestOwnershipClaimer_UnboundProviderFailsClosed(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *config.Config
		wantErr string
	}{
		{
			name:    "nil config",
			cfg:     nil,
			wantErr: "task provider identity is unbound",
		},
		{
			name:    "blank provider type",
			cfg:     &config.Config{TaskProvider: config.TaskProvider{Type: "   ", ProjectID: "p"}},
			wantErr: "task_provider.type is required",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := &Dispatcher{Config: c.cfg}
			got, err := d.ownershipClaimer()
			if err == nil {
				t.Fatalf("want fail-closed error, got claimer %T", got)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q must contain %q", err, c.wantErr)
			}
			// The default this replaces was specifically MemoryProvider's board.
			if strings.Contains(err.Error(), "memory") {
				t.Fatalf("error must not name a substituted board: %v", err)
			}
		})
	}
}

// TestBuildTaskPacket_CarriesProviderAndProjectBinding proves the packet names
// the activated board binding and forbids every other provider tool.
//
// FAC-145 (signed TaskContext) is not merged on origin/main; this binds from
// the same config the factory activates from (config.TaskProvider Type +
// ProjectID), which is the seam FAC-145's signed context replaces.
func TestBuildTaskPacket_CarriesProviderAndProjectBinding(t *testing.T) {
	task := &provider.Task{Ref: "FAC-155", Title: "Central provider factory"}
	lane := &config.LaneDef{Name: "worker", Prompt: ".herd/prompts/worker.md"}
	verification := config.Verification{TestCommand: "go test ./..."}

	for _, c := range []struct{ providerType, project string }{
		{"kaneo", "kaneo-project-id"},
		{"linear", "75548eb4-5382-444e-8dfd-6c6778f3b2d9"},
	} {
		t.Run(c.providerType, func(t *testing.T) {
			packet := buildTaskPacket(task, "herd/fac-155", ".herd/prompts/worker.md",
				c.providerType, c.project, lane, verification)

			if !strings.Contains(packet, "provider="+c.providerType) {
				t.Errorf("packet must bind the activated provider:\n%s", packet)
			}
			if !strings.Contains(packet, "project="+c.project) {
				t.Errorf("packet must bind the exact project ref:\n%s", packet)
			}
			if !strings.Contains(packet, "do NOT invoke any other provider CLI or API") {
				t.Errorf("packet must forbid alternative provider tools:\n%s", packet)
			}
		})
	}
}
