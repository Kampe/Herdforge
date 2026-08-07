package provider

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// ClaimStack is the production FAC-147 wiring: durable lease store +
// outbox + FencedCAS (ProviderCAS) + FencedBoard. cmd/herd, daemon, and
// dispatch open one of these so board mutations go through
// BeginProviderTransition/CompleteProviderTransition (and reclaim
// AdvanceFence) instead of bare TaskProvider writes.
//
// Minter is coordinator-only: loaded only when HERD_FENCE_BROKER_MINT_TOKEN
// is set. Workers never receive the mint secret; they present pre-minted
// capabilities only.
type ClaimStack struct {
	Dir     string
	Leases  *claim.SQLiteLeaseStore
	Outbox  *claim.SQLiteOutbox
	Fences  FenceStore
	CAS     *FencedCAS
	Board   *FencedBoard
	Manager *claim.ClaimManager
	TP      TaskProvider
	Minter  *FenceBrokerMinter // coordinator only; nil on workers
}

// CanonicalClaimDir resolves the single shared claim/fence/outbox directory.
// When HERD_CLAIM_DIR is set (fleet shared volume), that path is authoritative
// so fence-provision and every production command open the same directory.
// Otherwise: <canonical-root>/.herd/claim (never worktree-relative).
// override is typically $HERD_ROOT / $HERD_REPO_ROOT when HERD_CLAIM_DIR is unset.
func CanonicalClaimDir(startDir, override string) (string, error) {
	if claimDir := strings.TrimSpace(os.Getenv("HERD_CLAIM_DIR")); claimDir != "" {
		abs, err := filepath.Abs(claimDir)
		if err != nil {
			return "", fmt.Errorf("provider: HERD_CLAIM_DIR: %w", err)
		}
		return abs, nil
	}
	root, err := worktree.ResolveCanonicalRoot(context.Background(), startDir, override)
	if err != nil {
		return "", fmt.Errorf("provider: canonical claim dir: %w", err)
	}
	return filepath.Join(root, ".herd", "claim"), nil
}

// OpenCanonicalClaimStack opens the production claim stack. Prefers
// HERD_CLAIM_DIR when set so provisioned shared volumes are not ignored.
func OpenCanonicalClaimStack(tp TaskProvider) (*ClaimStack, error) {
	override := os.Getenv("HERD_ROOT")
	if override == "" {
		override = os.Getenv("HERD_REPO_ROOT")
	}
	dir, err := CanonicalClaimDir(".", override)
	if err != nil {
		return nil, err
	}
	return OpenClaimStack(dir, tp)
}

// isRealKaneoProvider reports whether tp is (or wraps) a KaneoProvider.
// Memory and other fakes are not real Kaneo and skip SHARED fencing.
func isRealKaneoProvider(tp TaskProvider) bool {
	switch UnwrapTaskProvider(tp).(type) {
	case *KaneoProvider:
		return true
	default:
		return false
	}
}

// OpenClaimStack opens (creating if needed) the durable claim/fence/outbox
// files under dir and wires a ClaimManager with WithProviderCAS +
// WithDurableOutbox over tp. Production callers must pass the path from
// CanonicalClaimDir / OpenCanonicalClaimStack — not a worktree-relative
// ".herd/claim".
func OpenClaimStack(dir string, tp TaskProvider) (*ClaimStack, error) {
	if tp == nil {
		return nil, fmt.Errorf("provider: OpenClaimStack requires a TaskProvider")
	}
	if dir == "" {
		return nil, fmt.Errorf("provider: OpenClaimStack requires a dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("provider: create claim stack dir: %w", err)
	}
	// Deployment invariant: shared fence high-water for real Kaneo.
	// AUTHORITY is durable advisory; SHARED+cluster provision token is the
	// multi-host identity that independent local dirs cannot mint (#1).
	authBody := []byte(
		"FAC-147 fence authority: this directory is the multi-process high-water boundary.\n" +
			"Set HERD_CLAIM_DIR to this path on every host (shared mount).\n" +
			"Provision once: HERD_CLAIM_DIR=... herd fence-provision\n" +
			"Fleet hosts: HERD_CLAIM_DIR + HERD_FENCE_VOLUME_ID from provision output.\n" +
			"Rotate only with HERD_FENCE_ROTATE=1 (authorized).\n",
	)
	authPath := filepath.Join(dir, "AUTHORITY")
	if err := os.WriteFile(authPath, authBody, 0o644); err != nil {
		return nil, fmt.Errorf("provider: write AUTHORITY: %w", err)
	}
	if af, err := os.Open(authPath); err != nil {
		return nil, fmt.Errorf("provider: reopen AUTHORITY: %w", err)
	} else {
		if err := af.Sync(); err != nil {
			_ = af.Close()
			return nil, fmt.Errorf("provider: fsync AUTHORITY: %w", err)
		}
		_ = af.Close()
	}
	if df, err := os.Open(dir); err != nil {
		return nil, fmt.Errorf("provider: open claim dir for AUTHORITY fsync: %w", err)
	} else {
		if err := df.Sync(); err != nil {
			_ = df.Close()
			return nil, fmt.Errorf("provider: fsync claim dir after AUTHORITY: %w", err)
		}
		_ = df.Close()
	}
	requireShared := os.Getenv("HERD_FENCE_REQUIRE_SHARED") == "1" ||
		os.Getenv("HERD_FENCE_FORCE_SHARED") == "1"
	// ALLOW_LOCAL is test-only: go test OR herd_test_seams InstallTestSeams.
	allowLocal := os.Getenv("HERD_FENCE_ALLOW_LOCAL") == "1" &&
		(testing.Testing() || claim.CurrentTestSeams() != nil)
	if isRealKaneoProvider(tp) && !allowLocal {
		if !testing.Testing() {
			requireShared = true
		}
	}
	if requireShared {
		if err := ValidateSharedMarker(dir); err != nil {
			return nil, fmt.Errorf("provider: shared fence authority required: %w", err)
		}
	}

	leasePath := filepath.Join(dir, "leases.db")
	outboxPath := filepath.Join(dir, "outbox.db")
	fencePath := filepath.Join(dir, "fences.db")

	leases, err := claim.NewSQLiteLeaseStore(leasePath)
	if err != nil {
		return nil, fmt.Errorf("provider: open lease store: %w", err)
	}
	outbox, err := claim.NewSQLiteOutbox(outboxPath)
	if err != nil {
		_ = leases.Close()
		return nil, fmt.Errorf("provider: open outbox: %w", err)
	}
	fences, err := NewSQLiteFenceStore(fencePath)
	if err != nil {
		_ = outbox.Close()
		_ = leases.Close()
		return nil, fmt.Errorf("provider: open fence store: %w", err)
	}
	// Production Kaneo CLI/HTTP have no native fence receiver: attach the
	// shared FenceStore as authoritative op/fence enforcer and refuse
	// unfenced mutates (FAC-147). No-op for MemoryProvider test stacks
	// that are not Kaneo.
	AttachAuthoritativeReceiver(tp, fences)
	cas, err := NewFencedCAS(fences, tp)
	if err != nil {
		_ = fences.Close()
		_ = outbox.Close()
		_ = leases.Close()
		return nil, err
	}
	board, err := NewFencedBoard(cas, tp)
	if err != nil {
		_ = cas.Close()
		_ = outbox.Close()
		_ = leases.Close()
		return nil, err
	}
	holdPath := filepath.Join(dir, "lifecycle-hold.db")
	holdAuth, herr := lifecycle.NewHoldAuthority(holdPath)
	if herr != nil {
		_ = fences.Close()
		_ = outbox.Close()
		_ = leases.Close()
		return nil, fmt.Errorf("provider: open hold authority: %w", herr)
	}
	opts := []claim.Option{
		claim.WithProviderCAS(cas),
		claim.WithDurableOutbox(outbox),
		claim.WithHoldReader(holdAuth),
	}
	// Injected test seams only (claim.InstallTestSeams) — never ambient env.
	if s := claim.CurrentTestSeams(); s != nil {
		if s.LeaseTTL > 0 {
			opts = append(opts, claim.WithTTL(s.LeaseTTL))
		}
		if s.ProviderLockTimeout > 0 {
			opts = append(opts, claim.WithProviderLockTimeout(s.ProviderLockTimeout))
		}
	}
	mgr := claim.NewClaimManager(leases, opts...)
	stack := &ClaimStack{
		Dir:     dir,
		Leases:  leases,
		Outbox:  outbox,
		Fences:  fences,
		CAS:     cas,
		Board:   board,
		Manager: mgr,
		TP:      tp,
	}
	// Never auto-load minter: claim-dir fence-mint.cred is same-UID readable and
	// is not an authority boundary (FAC-169). Coordinators must call
	// AttachCoordinatorMinter explicitly after a non-forgeable launch path.
	// Workers never receive minter.
	return stack, nil
}

// Close releases all durable stores. Safe on nil.
func (s *ClaimStack) Close() error {
	if s == nil {
		return nil
	}
	var first error
	if s.CAS != nil {
		if err := s.CAS.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.Outbox != nil {
		if err := s.Outbox.Close(); err != nil && first == nil {
			first = err
		}
	}
	if s.Leases != nil {
		if err := s.Leases.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// CanonicalRepoIdentity returns a stable absolute repo identity for
// LeaseKey.Repo so two worktrees of the same repository cannot mint
// independent generation-1 leases for the same card (FAC-147).
//
// Prefer git --git-common-dir from startDir so linked worktrees collide
// on the main repository root. HERD_ROOT / HERD_REPO_ROOT short-circuit
// only when startDir is empty or "." (process cwd) — never when the
// caller passes an explicit worktree path, or independent checkouts
// would falsely share identity via the env override.
func CanonicalRepoIdentity(startDir string) (string, error) {
	if startDir == "" {
		startDir = "."
	}
	override := ""
	if startDir == "." {
		override = os.Getenv("HERD_ROOT")
		if override == "" {
			override = os.Getenv("HERD_REPO_ROOT")
		}
	}
	root, err := worktree.ResolveCanonicalRoot(context.Background(), startDir, override)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil && resolved != "" {
		return resolved, nil
	}
	return root, nil
}

// LeaseKey builds the claim.LeaseKey with git-common-dir canonical Repo
// identity for every non-empty path (not just "."). Two registered
// worktrees of the same repository MUST produce identical keys.
func LeaseKey(repo, providerType, projectID, taskRef string) claim.LeaseKey {
	if repo == "" {
		repo = "."
	}
	if id, err := CanonicalRepoIdentity(repo); err == nil && id != "" {
		repo = id
	} else if abs, err := filepath.Abs(repo); err == nil {
		// Non-git fallback (tests without a repo): Abs+EvalSymlinks only.
		if resolved, err := filepath.EvalSymlinks(abs); err == nil {
			repo = resolved
		} else {
			repo = abs
		}
	}
	return claim.LeaseKey{
		Repo:     repo,
		Provider: providerType,
		Project:  projectID,
		TaskRef:  taskRef,
	}
}

// DefaultOwnerID is defined in owner.go (cryptographic process identity).

// ResolveTaskRole picks the exact role string ClaimManager.Claim will accept:
// a matching label when present, otherwise role (for unlabeled tasks).
// Prefer TaskOwnershipRole for production board mutations (pt5t7 #1).
func ResolveTaskRole(task *Task, role string) string {
	if task == nil {
		return role
	}
	if len(task.Labels) == 0 {
		return role
	}
	for _, l := range task.Labels {
		if strings.EqualFold(l, role) {
			return l
		}
	}
	return role
}

// RequireTaskRole returns the exact label matching role, or role when the
// task is unlabeled. Fail-closed when labels exist but none match role —
// never substitutes an arbitrary first label (hold c6ic8im #4).
func RequireTaskRole(task *Task, role string) (string, error) {
	if role == "" {
		return "", fmt.Errorf("provider: empty role")
	}
	if task == nil || len(task.Labels) == 0 {
		return role, nil
	}
	for _, l := range task.Labels {
		if strings.EqualFold(l, role) {
			return l, nil
		}
	}
	return "", fmt.Errorf("provider: role %q does not match task labels %v (refuse silent substitution)", role, task.Labels)
}

// knownImplementationRoles are durable task ownership labels used for lease
// claims. Session verbs like "reviewer" are NOT ownership roles.
// Unknown sole labels are NOT accepted (mid-repair #4).
var knownImplementationRoles = []string{"forge-smith", "worker", "builder", "coder"}

func isKnownImplementationRole(role string) bool {
	for _, want := range knownImplementationRoles {
		if strings.EqualFold(want, role) {
			return true
		}
	}
	return false
}

// TaskOwnershipRole resolves the durable implementation role for board
// mutations. Coordinator/reviewer sessions claim under this task role while
// ownerID carries the session identity.
//
// Order: preferred if it matches a label AND is a known implementation role
// (or preferred matches any label when preferred itself is known) → first
// known implementation label on the task → unlabeled uses preferred only if
// known → fail closed on unknown/sole-unknown labels.
func TaskOwnershipRole(task *Task, preferred string) (string, error) {
	if preferred != "" && !isKnownImplementationRole(preferred) {
		// preferred may be a session verb (reviewer); ignore for ownership
		// match and fall through to known labels on the task.
		preferred = ""
	}
	if task == nil || len(task.Labels) == 0 {
		if preferred == "" {
			return "", fmt.Errorf("provider: unlabeled task requires known preferred ownership role (%v)", knownImplementationRoles)
		}
		return preferred, nil
	}
	if preferred != "" {
		for _, l := range task.Labels {
			if strings.EqualFold(l, preferred) {
				return l, nil
			}
		}
	}
	for _, want := range knownImplementationRoles {
		for _, l := range task.Labels {
			if strings.EqualFold(l, want) {
				return l, nil
			}
		}
	}
	return "", fmt.Errorf("provider: no recognized implementation role in labels %v (known %v; refuse unknown sole-label ownership)", task.Labels, knownImplementationRoles)
}

// AcquireLease acquires a durable claim lease for taskRef. role and
// taskRole must satisfy ClaimManager.Claim's exact-match rules.
// Fail-closed: does not invent generations on conflict.
func (s *ClaimStack) AcquireLease(ctx context.Context, key claim.LeaseKey, ownerID, role, taskRole string) (*claim.Lease, error) {
	if s == nil || s.Manager == nil {
		return nil, fmt.Errorf("provider: nil ClaimStack")
	}
	if taskRole == "" {
		taskRole = role
	}
	// Canonical hold composite: lane + task under the claim role (FAC-119 hold authority).
	holdIDs := []lifecycle.HoldIdentity{
		{Repository: key.Repo, Owner: role, Lane: role, Scope: "lane"},
		{Repository: key.Repo, Owner: role, Lane: role, Task: key.TaskRef, Scope: "task"},
	}
	return s.Manager.Claim(ctx, claim.ClaimRequest{
		Key: key, OwnerID: ownerID, Role: role, TaskRole: taskRole,
		HoldIdentities: holdIDs,
	})
}

// MutateStatusGuarded is the production board status write: Claim must
// succeed (live lease), then AdvanceFence(taskID, generation) + Begin/
// Complete. On claim conflict the write is refused — contenders must not
// mint high+1 and preempt a live owner (FAC-147 audit fix).
func (s *ClaimStack) MutateStatusGuarded(
	ctx context.Context,
	key claim.LeaseKey,
	ownerID, role, taskRole, taskID, status string,
) (generation int64, err error) {
	if s == nil || s.Board == nil || s.Manager == nil || s.CAS == nil {
		return 0, fmt.Errorf("provider: incomplete ClaimStack")
	}
	lease, err := s.AcquireLease(ctx, key, ownerID, role, taskRole)
	if err != nil {
		return 0, fmt.Errorf("provider: refuse status mutation without live lease: %w", err)
	}
	// Release ONLY on clean success. Provider-success/local-failure and
	// ambiguous BLOCKED must keep the live lease so capability reconcile
	// can still pass lease re-check (FAC-147 root re-audit).
	var mutErr error
	defer func() {
		if mutErr != nil {
			err = mutErr
			return
		}
		if rerr := s.Manager.Release(ctx, key, ownerID, lease.Generation); rerr != nil {
			err = fmt.Errorf("provider: release lease after status mutation: %w", rerr)
			return
		}
		err = nil
	}()
	if err := s.CAS.AdvanceFence(ctx, taskID, lease.Generation); err != nil {
		mutErr = err
		return lease.Generation, mutErr
	}
	// Immutable per-call mint identity when a coordinator minter is attached
	// (stack.Minter and/or KaneoProvider.minter via AttachCoordinatorMinter).
	if k, ok := UnwrapTaskProvider(s.TP).(*KaneoProvider); ok && k != nil {
		if s.Minter != nil && k.minter == nil {
			_ = AttachCoordinatorMinter(k, s.Minter)
		}
		if k.minter != nil {
			ctx = WithMintIdentity(ctx, MintIdentity{
				Repo: lease.Repo, Provider: lease.Provider, Project: lease.Project,
				TaskRef: lease.TaskRef, OwnerID: lease.OwnerID,
			})
		}
	}
	if err := s.Board.MutateStatus(ctx, s.Manager, key, ownerID, lease.Generation, taskID, status); err != nil {
		mutErr = err
		return lease.Generation, mutErr
	}
	return lease.Generation, mutErr
}

// MutateClaimGuarded is the production ClaimTask path (status → in-progress)
// under a live lease generation via Begin/Complete. Fail-closed on claim conflict.
func (s *ClaimStack) MutateClaimGuarded(
	ctx context.Context,
	key claim.LeaseKey,
	ownerID, role, taskRole, taskID string,
) (*claim.Lease, error) {
	if s == nil || s.Board == nil || s.Manager == nil || s.CAS == nil {
		return nil, fmt.Errorf("provider: incomplete ClaimStack")
	}
	lease, err := s.AcquireLease(ctx, key, ownerID, role, taskRole)
	if err != nil {
		return nil, fmt.Errorf("provider: refuse board claim without live lease: %w", err)
	}
	if err := s.CAS.AdvanceFence(ctx, taskID, lease.Generation); err != nil {
		return lease, err
	}
	if k, ok := UnwrapTaskProvider(s.TP).(*KaneoProvider); ok && k != nil {
		if s.Minter != nil && k.minter == nil {
			_ = AttachCoordinatorMinter(k, s.Minter)
		}
		if k.minter != nil {
			ctx = WithMintIdentity(ctx, MintIdentity{
				Repo: lease.Repo, Provider: lease.Provider, Project: lease.Project,
				TaskRef: lease.TaskRef, OwnerID: lease.OwnerID,
			})
		}
	}
	if err := s.Board.MutateClaim(ctx, s.Manager, key, ownerID, lease.Generation, taskID, role); err != nil {
		return lease, err
	}
	return lease, nil
}

// IsClaimConflict reports whether err is an active-lease conflict.
func IsClaimConflict(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, claim.ErrAlreadyClaimed) {
		return true
	}
	var conflict *claim.ClaimConflictError
	return errors.As(err, &conflict)
}
