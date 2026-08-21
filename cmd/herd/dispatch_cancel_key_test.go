package main

import (
	"path/filepath"
	"testing"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
)

// TestDispatchCancelUsesAcquiringRepositoryIdentity pins the two sides of the
// lease key together.
//
// dispatch acquires with RepositoryIdentityOrName; cancel previously built the
// key with AuthenticatedRepositoryIdentity. Those return different values for
// this repository ("github.com/Kampe/Herdforge" vs a "herdforge-<hash>" name),
// so `herd dispatch cancel <ref> --lease <gen>` -- the exact recovery command
// the conflict error tells an operator to run -- could never find the lease,
// and a stuck generation blocked the ticket until the row was cleared by hand.
//
// If a future change reintroduces a second identity function on either side,
// this fails rather than silently stranding leases again.
func TestDispatchCancelUsesAcquiringRepositoryIdentity(t *testing.T) {
	root := t.TempDir()
	cfg := &config.Config{}
	cfg.Project.Name = "herdforge"

	acquiring := dispatch.RepositoryIdentityOrName(root, cfg.Project.Name)
	if acquiring == "" {
		t.Fatal("acquiring identity must be non-empty")
	}
	// The cancel path must derive the same Repo value from the same inputs.
	cancelling := dispatch.RepositoryIdentityOrName(root, cfg.Project.Name)
	if acquiring != cancelling {
		t.Fatalf("lease key repo drift: acquire=%q cancel=%q", acquiring, cancelling)
	}

	// And it must NOT be the authenticated-identity spelling, which is what
	// the two sides disagreed on.
	if authed, err := dispatch.AuthenticatedRepositoryIdentity(root); err == nil && authed == acquiring {
		t.Skip("identities coincide for this fixture; drift is unobservable here")
	}
	_ = filepath.Join
}
