package security

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// ClaimManagerLookup adapts claim.ClaimManager to LiveClaimLookup (FAC-147).
type ClaimManagerLookup struct {
	M *claim.ClaimManager
}

func (c ClaimManagerLookup) LookupActiveClaim(ctx context.Context, taskRef string) (*LiveClaimRecord, error) {
	if c.M == nil {
		return nil, fmt.Errorf("%w: nil claim manager", ErrLeaseNotLive)
	}
	claims, err := c.M.ActiveClaims(ctx)
	if err != nil {
		return nil, err
	}
	taskRef = strings.TrimSpace(taskRef)
	now := time.Now()
	for _, l := range claims {
		if l == nil || l.TaskRef != taskRef {
			continue
		}
		if l.Status != claim.StatusActive {
			continue
		}
		if l.Expired(now) {
			continue
		}
		return &LiveClaimRecord{
			TaskRef:    l.TaskRef,
			Generation: l.Generation,
			OwnerID:    l.OwnerID,
			Role:       l.Role,
			ExpiresAt:  l.ExpiresAt,
		}, nil
	}
	return nil, fmt.Errorf("%w: no active claim for %s", ErrLeaseNotLive, taskRef)
}

// claimAuthority is the exact FAC-147 production seam.
// Nothing assumes .herd/claims.db or ambient generation alone.
// Production wires the merged FAC-147 authority via RegisterClaimAuthority.
var (
	claimAuthMu   sync.Mutex
	claimAuthority LiveClaimLookup
)

// RegisterClaimAuthority installs the canonical FAC-147 live-claim lookup.
// Call this from the process that owns the merged claim/fence implementation
// after FAC-147 lands. Until then, task launches fail closed.
func RegisterClaimAuthority(l LiveClaimLookup) {
	claimAuthMu.Lock()
	claimAuthority = l
	claimAuthMu.Unlock()
}

// CanonicalLeaseDBPath is the exact FAC-147 lease store path.
// Do not open a parallel <root>/.herd/claims.db authority.
func CanonicalLeaseDBPath(root string) string {
	if p := strings.TrimSpace(os.Getenv("HERD_CLAIMS_DB")); p != "" {
		return p
	}
	if root == "" {
		root = "."
	}
	// Canonical stack: .herd/claim/leases.db (not claims.db).
	return filepath.Join(root, ".herd", "claim", "leases.db")
}

// WireCanonicalClaimAuthority opens the exact FAC-147 SQLite lease store
// at <root>/.herd/claim/leases.db (or $HERD_CLAIMS_DB) and registers
// ClaimManagerLookup. Never invents a parallel empty claims.db.
func WireCanonicalClaimAuthority(root string) error {
	path := CanonicalLeaseDBPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	store, err := claim.NewSQLiteLeaseStore(path)
	if err != nil {
		return fmt.Errorf("FAC-147 claim authority open %s: %w", path, err)
	}
	mgr := claim.NewClaimManager(store)
	RegisterClaimAuthority(ClaimManagerLookup{M: mgr})
	return nil
}

// ClearClaimAuthority removes the registered authority (tests).
func ClearClaimAuthority() {
	claimAuthMu.Lock()
	claimAuthority = nil
	claimAuthMu.Unlock()
}

// testClaimLookup is a test-only inject (SetTestClaimLookup).
var (
	testClaimLookupMu sync.Mutex
	testClaimLookup   LiveClaimLookup
)

// SetTestClaimLookup injects a LiveClaimLookup for tests. Production never calls this.
func SetTestClaimLookup(l LiveClaimLookup) (restore func()) {
	testClaimLookupMu.Lock()
	prev := testClaimLookup
	testClaimLookup = l
	testClaimLookupMu.Unlock()
	return func() {
		testClaimLookupMu.Lock()
		testClaimLookup = prev
		testClaimLookupMu.Unlock()
	}
}

// ResolveClaimLookup returns test inject, then registered FAC-147 authority.
// It does NOT open a parallel assumed claims.db store.
func ResolveClaimLookup() LiveClaimLookup {
	testClaimLookupMu.Lock()
	if testClaimLookup != nil {
		l := testClaimLookup
		testClaimLookupMu.Unlock()
		return l
	}
	testClaimLookupMu.Unlock()

	claimAuthMu.Lock()
	l := claimAuthority
	claimAuthMu.Unlock()
	return l
}

// RequireClaimAuthority returns the live lookup or a fail-closed error for tasks.
func RequireClaimAuthority() (LiveClaimLookup, error) {
	l := ResolveClaimLookup()
	if l == nil {
		return nil, fmt.Errorf("%w: FAC-147 claim authority not registered (cannot validate live lease)", ErrLeaseNotLive)
	}
	return l, nil
}
