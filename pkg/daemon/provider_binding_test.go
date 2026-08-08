package daemon

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// TestOwnershipClaimer_UnboundProviderFailsClosed is the daemon half of
// FAC-155's lease-identity fix. The daemon and the dispatcher must agree on the
// board a launch lease is keyed to; both previously substituted "memory" for an
// absent/blank provider type, so an unconfigured engine leased against a board
// nobody activated.
//
// Non-vacuous: restoring `providerType := "memory"` lets both rows build a real
// LeaseOwnership instead of erroring.
func TestOwnershipClaimer_UnboundProviderFailsClosed(t *testing.T) {
	// Pin repository identity so the assertions below can only be about
	// provider binding, never about git state in the test environment.
	orig := daemonAuthenticatedRepositoryIdentity
	daemonAuthenticatedRepositoryIdentity = func(string) (string, error) {
		return "github.com/Kampe/Herdforge", nil
	}
	t.Cleanup(func() { daemonAuthenticatedRepositoryIdentity = orig })

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
			cfg:     &config.Config{TaskProvider: config.TaskProvider{Type: "  ", ProjectID: "p"}},
			wantErr: "task_provider.type is required",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := &Engine{Config: c.cfg}
			got, err := e.ownershipClaimer()
			if err == nil {
				t.Fatalf("want fail-closed error, got claimer %T", got)
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("error %q must contain %q", err, c.wantErr)
			}
			if strings.Contains(err.Error(), "memory") {
				t.Fatalf("error must not name a substituted board: %v", err)
			}
		})
	}

	// A configured engine must still get a claimer, and the lease must carry
	// the configured board identity — otherwise the rows above would pass for
	// the wrong reason (a blanket refusal), and nothing would prove the
	// provider binding actually reaches the lease key.
	//
	// Rooted in a temp git repo: this branch opens a real lease store, which
	// must never land in the package directory (cwd-pollution guards).
	root := t.TempDir()
	if out, err := exec.Command("git", "-C", root, "init").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	e := &Engine{
		Config: &config.Config{TaskProvider: config.TaskProvider{
			Type: "linear", ProjectID: "p",
		}},
		Worktree: &worktree.WorktreeManager{RepoRoot: root},
	}
	claimer, err := e.ownershipClaimer()
	if err != nil {
		t.Fatalf("configured engine must get a claimer: %v", err)
	}
	own, ok := claimer.(*deps.LeaseOwnership)
	if !ok {
		t.Fatalf("claimer=%T, want *deps.LeaseOwnership", claimer)
	}
	t.Cleanup(func() { _ = own.Close() })
	if own.Provider != "linear" || own.Project != "p" {
		t.Fatalf("lease identity=%q/%q, want linear/p", own.Provider, own.Project)
	}
}
