package deps

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

// ErrNotOwner means compensation was refused — this owner+generation no longer
// holds the active cross-process lease.
var ErrNotOwner = errors.New("deps: not lease owner/generation")

// ErrAlreadyClaimed means the SQLite lease is held by another owner.
var ErrAlreadyClaimed = errors.New("deps: task already leased")

// OwnershipToken is the exclusive launch identity: claim package owner +
// generation, bound to the selection graph revision. Compensation is fenced
// by generation (not a process-local map).
type OwnershipToken struct {
	Key         claim.LeaseKey
	TaskID      TaskID
	TaskRef     Ref
	OwnerID     string
	Generation  int64
	GraphRev    string // relation-graph revision at selection (pre-claim)
	ProviderRev string
	Role        string
	ClaimedAt   time.Time
}

// OwnershipClaimer acquires a durable cross-process lease before side effects.
// The production implementation wraps claim.ClaimManager + SQLiteLeaseStore.
//
// Failure order (hard invariant — FAC-159 audit h5d6pay5vamxvv277qtt5qmk):
//  1. StillOwns (owner+generation)
//  2. exactly-one durable lifecycle compensate (outbox) while still owned
//  3. ReleaseIfOwner only after durable compensate succeeds/readbacks
//
// On durable compensate failure: retain the lease (Recovering) — never release
// then compensate (B can acquire and get stomped by stale A).
type OwnershipClaimer interface {
	ClaimExclusive(ctx context.Context, taskID TaskID, taskRef Ref, role, graphRev, providerRev, worktreeHint string) (*OwnershipToken, error)
	StillOwns(ctx context.Context, tok *OwnershipToken) (bool, error)
	// ReleaseIfOwner drops the generation fence only when owner+generation still
	// match. Call AFTER successful durable compensation — never before.
	ReleaseIfOwner(ctx context.Context, tok *OwnershipToken, reason string) error
	Close() error
}

// LeaseOwnership is the admissible fence: SQLite-backed claim.ClaimManager.
// Two independent ClaimManager instances against the same DB path (two OS
// processes) race to exactly one winner — proven by claim package tests and
// deps.TestLeaseOwnership_TwoIndependentManagers_ExactlyOneWinner.
type LeaseOwnership struct {
	CM           *claim.ClaimManager
	Store        claim.LeaseStore // for Close when we own it
	Repo         string
	Provider     string
	Project      string
	LaneResolver func(string) (string, error)
	closeDB      func() error
}

// OpenLeaseOwnership opens (or creates) a SQLite lease DB at path and returns
// a ClaimManager-backed OwnershipClaimer. path should be repo-relative under
// .herd/ (e.g. .herd/launch-claims.db) resolved by the caller to an absolute
// filesystem path for SQLite only — never embed absolute paths in configs.
func OpenLeaseOwnership(dbPath, repo, provider, project string) (*LeaseOwnership, error) {
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return nil, err
	}
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("deps: open launch lease store: %w", err)
	}
	holdDB, canonicalErr := lifecycle.CanonicalStatePathForLaunchDB(dbPath)
	if canonicalErr != nil {
		if filepath.Base(filepath.Dir(dbPath)) == ".herd" {
			_ = store.Close()
			return nil, fmt.Errorf("deps: canonical hold path: %w", canonicalErr)
		}
		holdDB = lifecycle.CanonicalStatePath(filepath.Dir(filepath.Dir(dbPath)))
	}
	if err := os.MkdirAll(filepath.Dir(holdDB), 0o755); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("deps: create canonical hold directory: %w", err)
	}
	holds, err := lifecycle.NewHoldAuthority(holdDB)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("deps: open canonical hold authority: %w", err)
	}
	return &LeaseOwnership{
		CM:       claim.NewClaimManager(store, claim.WithHoldReader(holds)),
		Store:    store,
		Repo:     repo,
		Provider: provider,
		Project:  project,
		closeDB:  func() error { _ = holds.Close(); return store.Close() },
	}, nil
}

// NewLeaseOwnership wraps an existing ClaimManager (tests inject two managers
// on independent SQLite stores sharing one file path).
func NewLeaseOwnership(cm *claim.ClaimManager, repo, provider, project string) *LeaseOwnership {
	return &LeaseOwnership{CM: cm, Repo: repo, Provider: provider, Project: project}
}

func (o *LeaseOwnership) key(taskRef Ref) claim.LeaseKey {
	return claim.LeaseKey{
		Repo:     o.Repo,
		Provider: o.Provider,
		Project:  o.Project,
		TaskRef:  string(taskRef),
	}
}

func (o *LeaseOwnership) canonicalLane(role string) (string, error) {
	if o.LaneResolver == nil {
		return "", fmt.Errorf("canonical lane resolver is required")
	}
	lane, err := o.LaneResolver(role)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(lane) == "" {
		return "", fmt.Errorf("unknown configured role %q", role)
	}
	return strings.TrimSpace(lane), nil
}

func newOwnerID(role string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	if role == "" {
		role = "launch"
	}
	return fmt.Sprintf("%s-pid%d-%s", role, os.Getpid(), hex.EncodeToString(b[:]))
}

// ClaimExclusive acquires a durable generation-fenced lease. graphRev must be
// non-empty (selection relation revision). Provider board status is NOT
// mutated here — the lease is the ownership boundary until FAC-147 CAS.
func (o *LeaseOwnership) ClaimExclusive(ctx context.Context, taskID TaskID, taskRef Ref, role, graphRev, providerRev, worktreeHint string) (*OwnershipToken, error) {
	if o == nil || o.CM == nil {
		return nil, fmt.Errorf("deps: lease ownership not configured")
	}
	if !taskRef.Valid() {
		return nil, fmt.Errorf("deps: claim requires task ref")
	}
	if graphRev == "" {
		return nil, fmt.Errorf("%w: graph revision required for claim", ErrClaimFence)
	}
	if role == "" {
		role = "launch"
	}
	// claim.Claim requires Role == TaskRole and non-empty TaskRole.
	ownerID := newOwnerID(role)
	key := o.key(taskRef)
	laneName, err := o.canonicalLane(role)
	if err != nil {
		return nil, err
	}
	lease, err := o.CM.Claim(ctx, claim.ClaimRequest{
		Key:            key,
		OwnerID:        ownerID,
		Role:           role,
		TaskRole:       role,
		WorktreePath:   worktreeHint,
		HoldIdentity:   lifecycle.HoldIdentity{Repository: o.Repo, Owner: role, Lane: laneName, Task: string(taskRef), Scope: "task"},
		HoldIdentities: []lifecycle.HoldIdentity{{Repository: o.Repo, Owner: role, Lane: laneName, Scope: "lane"}, {Repository: o.Repo, Owner: role, Lane: laneName, Task: string(taskRef), Scope: "task"}},
	})
	if err != nil {
		var conflict *claim.ClaimConflictError
		if errors.As(err, &conflict) || errors.Is(err, claim.ErrAlreadyClaimed) {
			return nil, fmt.Errorf("%w: %v", ErrAlreadyClaimed, err)
		}
		return nil, err
	}
	return &OwnershipToken{
		Key:         key,
		TaskID:      taskID,
		TaskRef:     taskRef,
		OwnerID:     lease.OwnerID,
		Generation:  lease.Generation,
		GraphRev:    graphRev,
		ProviderRev: providerRev,
		Role:        role,
		ClaimedAt:   lease.ClaimedAt,
	}, nil
}

func (o *LeaseOwnership) StillOwns(ctx context.Context, tok *OwnershipToken) (bool, error) {
	if o == nil || o.CM == nil || tok == nil {
		return false, nil
	}
	claims, err := o.CM.ActiveClaims(ctx)
	if err != nil {
		return false, err
	}
	for _, l := range claims {
		if l.LeaseKey == tok.Key && l.OwnerID == tok.OwnerID && l.Generation == tok.Generation {
			return true, nil
		}
	}
	return false, nil
}

// ReleaseIfOwner releases the generation-fenced lease only while this token
// still matches active owner+generation. Stale tokens (after B acquires) get
// ErrNotOwner and must not mutate B's lifecycle.
func (o *LeaseOwnership) ReleaseIfOwner(ctx context.Context, tok *OwnershipToken, reason string) error {
	if o == nil || tok == nil {
		return fmt.Errorf("deps: nil lease release")
	}
	owns, err := o.StillOwns(ctx, tok)
	if err != nil {
		return err
	}
	if !owns {
		return fmt.Errorf("%w: refuse release (%s) for %s g%d", ErrNotOwner, reason, tok.TaskRef, tok.Generation)
	}
	if err := o.CM.Release(ctx, tok.Key, tok.OwnerID, tok.Generation); err != nil {
		if errors.Is(err, claim.ErrStaleGeneration) || errors.Is(err, claim.ErrNotFound) {
			return fmt.Errorf("%w: %v", ErrNotOwner, err)
		}
		return fmt.Errorf("deps: lease release: %w", err)
	}
	return nil
}

// CompensateIfOwner is a deprecated name for ReleaseIfOwner kept as a thin
// wrapper for in-flight callers. Prefer ReleaseIfOwner — "compensate" is the
// durable outbox step, not the lease drop.
func (o *LeaseOwnership) CompensateIfOwner(ctx context.Context, tok *OwnershipToken, reason string) error {
	return o.ReleaseIfOwner(ctx, tok, reason)
}

func (o *LeaseOwnership) Close() error {
	if o != nil && o.closeDB != nil {
		return o.closeDB()
	}
	return nil
}

// TwoIndependentManagersClaim races two ClaimManagers on the same SQLite
// path (two *sql.DB, like two OS processes). Exactly one ClaimExclusive wins.
func TwoIndependentManagersClaim(ctx context.Context, dbPath, repo, provider, project string, taskRef Ref, graphRev string) (wins, conflicts int, err error) {
	a, err := OpenLeaseOwnership(dbPath, repo, provider, project)
	if err != nil {
		return 0, 0, err
	}
	defer a.Close()
	b, err := OpenLeaseOwnership(dbPath, repo, provider, project)
	if err != nil {
		return 0, 0, err
	}
	defer b.Close()

	type res struct {
		err error
	}
	ch := make(chan res, 2)
	go func() {
		_, e := a.ClaimExclusive(ctx, "id", taskRef, "launch", graphRev, "", "/wt-a")
		ch <- res{e}
	}()
	go func() {
		_, e := b.ClaimExclusive(ctx, "id", taskRef, "launch", graphRev, "", "/wt-b")
		ch <- res{e}
	}()
	r1, r2 := <-ch, <-ch
	for _, r := range []res{r1, r2} {
		if r.err == nil {
			wins++
		} else if errors.Is(r.err, ErrAlreadyClaimed) {
			conflicts++
		} else {
			return wins, conflicts, r.err
		}
	}
	if wins != 1 || conflicts != 1 {
		return wins, conflicts, fmt.Errorf("want exactly one winner, got wins=%d conflicts=%d", wins, conflicts)
	}
	return wins, conflicts, nil
}

// DefaultLaunchLeasePath returns the conventional relative lease DB path.
func DefaultLaunchLeasePath() string {
	return filepath.Join(".herd", "launch-claims.db")
}

// Ensure absolute for SQLite open from a known repo root without writing
// absolute paths into config files.
func ResolveLaunchLeasePath(repoRoot string) string {
	if repoRoot == "" {
		repoRoot = "."
	}
	return filepath.Join(repoRoot, DefaultLaunchLeasePath())
}

// Must not leave unused imports.
var _ = time.Now
