package claim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/lifecycle"
)

var (
	ErrDualCancelWrongOwner        = errors.New("claim: dual-cancel wrong owner")
	ErrDualCancelWrongGeneration   = errors.New("claim: dual-cancel wrong generation")
	ErrDualCancelLiveWorker        = errors.New("claim: dual-cancel live non-expired worker ownership")
	ErrDualCancelHeld              = errors.New("claim: dual-cancel held scope")
	ErrDualCancelProviderLock      = errors.New("claim: dual-cancel provider lock")
	ErrDualCancelAmbiguousIdentity = errors.New("claim: dual-cancel ambiguous identity")
	ErrDualCancelPartialStore      = errors.New("claim: dual-cancel partial store access")
	ErrDualCancelRecoverable       = errors.New("claim: dual-cancel incomplete; journal is recoverable")
)

const CoordinatorDispatchOwner = "coordinator-dispatch"

// Disposition is the durable outcome of one store at one generation.
type Disposition string

const (
	DispositionReleased        Disposition = "released"
	DispositionAlreadyReleased Disposition = "already-released"
	DispositionAbsent          Disposition = "absent"
	DispositionUnchanged       Disposition = "unchanged"
)

// StoreReport is CLI-safe: store path, task ref, generation, disposition.
type StoreReport struct {
	Store       string
	TaskRef     string
	Generation  int64
	Disposition Disposition
}

// DualCancelResult is the exact readback of both stores after cancel.
type DualCancelResult struct {
	Coordinator StoreReport
	Launch      StoreReport
}

// DualCancelRequest is one coordinator-only generation-fenced dual-store
// cancellation. Owner is the coordinator-dispatch identity. Launch-claim
// owners are observed from the matching generation row, never guessed.
type DualCancelRequest struct {
	Key             LeaseKey
	Owner           string
	Generation      int64
	Now             time.Time
	Coordinator     LeaseStore
	Launch          LeaseStore
	CoordinatorPath string
	LaunchPath      string
	JournalPath     string
	HoldReader      lifecycle.HoldReader
	// AfterFirstRelease is a test-only seam that simulates a crash after
	// the first planned store release and before the second.
	AfterFirstRelease func() error
}

type dualSnapshotStore interface {
	LeasesByGeneration(context.Context, LeaseKey, int64) ([]*Lease, error)
	CurrentLease(context.Context, LeaseKey) (*Lease, error)
	ProviderLockHeld(context.Context, int64) (bool, error)
	PeekLatestGeneration(context.Context, LeaseKey) (int64, error)
}

type dualCancelJournal struct {
	Repo            string `json:"repo"`
	Provider        string `json:"provider"`
	Project         string `json:"project"`
	TaskRef         string `json:"task_ref"`
	Owner           string `json:"owner"`
	Generation      int64  `json:"generation"`
	ReleaseCoord    bool   `json:"release_coordinator"`
	ReleaseLaunch   bool   `json:"release_launch"`
	CoordinatorDone bool   `json:"coordinator_done"`
	LaunchDone      bool   `json:"launch_done"`
	State           string `json:"state"`
}

var dispatchClaimOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+-pid[0-9]+-[0-9a-f]{16}$`)

func (r DualCancelRequest) coordinatorName() string {
	if strings.TrimSpace(r.CoordinatorPath) != "" {
		return r.CoordinatorPath
	}
	return "coordinator-store"
}

func (r DualCancelRequest) launchName() string {
	if strings.TrimSpace(r.LaunchPath) != "" {
		return r.LaunchPath
	}
	return "launch-store"
}

// CancelMatchingGeneration releases the exact matching generation in the
// coordinator-dispatch store and the launch-claim store. It never mints a
// generation. A second-store failure leaves a recoverable journal rather
// than a silent split.
func CancelMatchingGeneration(ctx context.Context, req DualCancelRequest) (*DualCancelResult, error) {
	if req.Coordinator == nil || req.Launch == nil {
		return nil, fmt.Errorf("%w: both stores are required", ErrDualCancelPartialStore)
	}
	if strings.TrimSpace(req.Owner) == "" || req.Generation < 1 {
		return nil, fmt.Errorf("%w: owner and generation are required", ErrDualCancelWrongGeneration)
	}
	if strings.TrimSpace(req.Key.Repo) == "" || strings.TrimSpace(req.Key.Provider) == "" || strings.TrimSpace(req.Key.Project) == "" || strings.TrimSpace(req.Key.TaskRef) == "" {
		return nil, fmt.Errorf("%w: lease key is incomplete", ErrDualCancelAmbiguousIdentity)
	}
	now := req.Now
	if now.IsZero() {
		now = time.Now()
	}
	coordSnap, ok := req.Coordinator.(dualSnapshotStore)
	if !ok {
		return nil, fmt.Errorf("%w: coordinator store cannot snapshot by generation", ErrDualCancelPartialStore)
	}
	launchSnap, ok := req.Launch.(dualSnapshotStore)
	if !ok {
		return nil, fmt.Errorf("%w: launch store cannot snapshot by generation", ErrDualCancelPartialStore)
	}

	pending, err := loadDualCancelJournal(req.JournalPath, req.Key, req.Owner, req.Generation)
	if err != nil {
		return nil, err
	}

	coordRows, err := coordSnap.LeasesByGeneration(ctx, req.Key, req.Generation)
	if err != nil {
		return nil, fmt.Errorf("%w: coordinator read: %v", ErrDualCancelPartialStore, err)
	}
	launchRows, err := launchSnap.LeasesByGeneration(ctx, req.Key, req.Generation)
	if err != nil {
		return nil, fmt.Errorf("%w: launch read: %v", ErrDualCancelPartialStore, err)
	}
	if len(coordRows) > 1 || len(launchRows) > 1 {
		return nil, fmt.Errorf("%w: %s generation %d", ErrDualCancelAmbiguousIdentity, req.Key.TaskRef, req.Generation)
	}

	coordRow := firstLease(coordRows)
	launchRow := firstLease(launchRows)

	if pending == nil {
		if err := refuseWrongGeneration(ctx, coordSnap, launchSnap, req.Key, req.Generation, coordRow, launchRow); err != nil {
			return nil, err
		}
		if err := evaluateCoordinatorFence(ctx, coordSnap, coordRow, req.Owner, req.Generation); err != nil {
			return nil, err
		}
		if err := evaluateLaunchFence(ctx, launchSnap, launchRow, req, now); err != nil {
			return nil, err
		}
		if err := evaluateHolds(ctx, req.HoldReader, coordRow, launchRow); err != nil {
			return nil, err
		}
	}

	releaseCoord := coordRow != nil && coordRow.Status == StatusActive
	releaseLaunch := launchRow != nil && launchRow.Status == StatusActive
	if pending != nil {
		releaseCoord = pending.ReleaseCoord && !pending.CoordinatorDone
		releaseLaunch = pending.ReleaseLaunch && !pending.LaunchDone
	}

	journal := dualCancelJournal{
		Repo: req.Key.Repo, Provider: req.Key.Provider, Project: req.Key.Project,
		TaskRef: req.Key.TaskRef, Owner: req.Owner, Generation: req.Generation,
		ReleaseCoord:    coordRow != nil && coordRow.Status == StatusActive,
		ReleaseLaunch:   launchRow != nil && launchRow.Status == StatusActive,
		CoordinatorDone: pending != nil && pending.CoordinatorDone,
		LaunchDone:      pending != nil && pending.LaunchDone,
		State:           "intent",
	}
	if pending != nil {
		journal = *pending
		journal.State = "intent"
	}
	if releaseCoord || releaseLaunch {
		if err := writeDualCancelJournal(req.JournalPath, journal); err != nil {
			return nil, err
		}
	}

	firstDone := false
	apply := func(store LeaseStore, row **Lease, release bool, markDone func(*dualCancelJournal)) error {
		if !release {
			return nil
		}
		if *row == nil {
			return fmt.Errorf("%w: planned row disappeared", ErrDualCancelPartialStore)
		}
		if err := releaseExactRow(ctx, store, req.HoldReader, *row, now); err != nil {
			return err
		}
		markDone(&journal)
		journal.State = "partial"
		if err := writeDualCancelJournal(req.JournalPath, journal); err != nil {
			return err
		}
		if !firstDone && req.AfterFirstRelease != nil {
			firstDone = true
			if hookErr := req.AfterFirstRelease(); hookErr != nil {
				return fmt.Errorf("%w: %v", ErrDualCancelRecoverable, hookErr)
			}
		}
		firstDone = true
		return nil
	}

	if err := apply(req.Launch, &launchRow, releaseLaunch, func(j *dualCancelJournal) { j.LaunchDone = true }); err != nil {
		return nil, err
	}
	if err := apply(req.Coordinator, &coordRow, releaseCoord, func(j *dualCancelJournal) { j.CoordinatorDone = true }); err != nil {
		if errors.Is(err, ErrDualCancelRecoverable) {
			return nil, err
		}
		if journal.LaunchDone && !journal.CoordinatorDone {
			return nil, fmt.Errorf("%w: %v", ErrDualCancelRecoverable, err)
		}
		return nil, err
	}

	coordAfter, err := exactReadback(ctx, coordSnap, req.Key, req.Generation)
	if err != nil {
		return nil, fmt.Errorf("%w: coordinator readback: %v", ErrDualCancelPartialStore, err)
	}
	launchAfter, err := exactReadback(ctx, launchSnap, req.Key, req.Generation)
	if err != nil {
		return nil, fmt.Errorf("%w: launch readback: %v", ErrDualCancelPartialStore, err)
	}
	if err := assertReleasedOrAbsent(coordAfter, journal.ReleaseCoord || (pending != nil && pending.ReleaseCoord)); err != nil {
		return nil, fmt.Errorf("coordinator %w", err)
	}
	if err := assertReleasedOrAbsent(launchAfter, journal.ReleaseLaunch || (pending != nil && pending.ReleaseLaunch)); err != nil {
		return nil, fmt.Errorf("launch %w", err)
	}

	journal.State = "done"
	journal.CoordinatorDone = true
	journal.LaunchDone = true
	if err := writeDualCancelJournal(req.JournalPath, journal); err != nil {
		return nil, err
	}

	result := &DualCancelResult{
		Coordinator: reportFor(req.coordinatorName(), req.Key.TaskRef, req.Generation, coordRow, coordAfter),
		Launch:      reportFor(req.launchName(), req.Key.TaskRef, req.Generation, launchRow, launchAfter),
	}
	if coordAfter != nil && coordAfter.Status == StatusActive && coordAfter.Generation != req.Generation {
		result.Coordinator.Disposition = DispositionUnchanged
	}
	return result, nil
}

func firstLease(rows []*Lease) *Lease {
	if len(rows) == 0 {
		return nil
	}
	return rows[0]
}

func refuseWrongGeneration(ctx context.Context, coord, launch dualSnapshotStore, key LeaseKey, generation int64, coordRow, launchRow *Lease) error {
	if coordRow != nil || launchRow != nil {
		return nil
	}
	coordLatest, err := coord.PeekLatestGeneration(ctx, key)
	if err != nil {
		return fmt.Errorf("%w: coordinator high-water: %v", ErrDualCancelPartialStore, err)
	}
	launchLatest, err := launch.PeekLatestGeneration(ctx, key)
	if err != nil {
		return fmt.Errorf("%w: launch high-water: %v", ErrDualCancelPartialStore, err)
	}
	if coordLatest == 0 && launchLatest == 0 {
		return fmt.Errorf("%w: no lease for %s generation %d", ErrDualCancelWrongGeneration, key.TaskRef, generation)
	}
	return fmt.Errorf("%w: %s generation %d is not current (coordinator high-water %d, launch high-water %d)", ErrDualCancelWrongGeneration, key.TaskRef, generation, coordLatest, launchLatest)
}

func evaluateCoordinatorFence(ctx context.Context, snap dualSnapshotStore, row *Lease, owner string, generation int64) error {
	if row == nil {
		return nil
	}
	if row.OwnerID != owner {
		return fmt.Errorf("%w: coordinator generation %d owner mismatch", ErrDualCancelWrongOwner, generation)
	}
	if row.Status != StatusActive && row.Status != StatusReleased {
		return fmt.Errorf("%w: coordinator generation %d is %s", ErrDualCancelWrongGeneration, generation, row.Status)
	}
	if row.Status == StatusActive {
		held, err := snap.ProviderLockHeld(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("%w: coordinator provider lock: %v", ErrDualCancelPartialStore, err)
		}
		if held {
			return fmt.Errorf("%w: coordinator %s generation %d", ErrDualCancelProviderLock, row.TaskRef, generation)
		}
	}
	return nil
}

func evaluateLaunchFence(ctx context.Context, snap dualSnapshotStore, row *Lease, req DualCancelRequest, now time.Time) error {
	if row == nil {
		return nil
	}
	if row.Status != StatusActive && row.Status != StatusReleased {
		return fmt.Errorf("%w: launch generation %d is %s", ErrDualCancelWrongGeneration, req.Generation, row.Status)
	}
	if row.Status != StatusActive {
		return nil
	}
	held, err := snap.ProviderLockHeld(ctx, row.ID)
	if err != nil {
		return fmt.Errorf("%w: launch provider lock: %v", ErrDualCancelPartialStore, err)
	}
	if held {
		return fmt.Errorf("%w: launch %s generation %d", ErrDualCancelProviderLock, row.TaskRef, req.Generation)
	}
	if liveWorkerOwnership(row, req.Owner, now) {
		return fmt.Errorf("%w: %s generation %d", ErrDualCancelLiveWorker, row.TaskRef, req.Generation)
	}
	return nil
}

func liveWorkerOwnership(row *Lease, coordinatorOwner string, now time.Time) bool {
	if row == nil || row.Status != StatusActive || row.Expired(now) {
		return false
	}
	if row.OwnerID == coordinatorOwner || strings.HasPrefix(row.OwnerID, "coordinator-") {
		return false
	}
	if dispatchClaimOwnerPattern.MatchString(row.OwnerID) {
		return false
	}
	return true
}

func evaluateHolds(ctx context.Context, reader lifecycle.HoldReader, rows ...*Lease) error {
	fencer, ok := reader.(interface {
		WithUnheldTransition(context.Context, []lifecycle.HoldIdentity, func() error) error
	})
	for _, row := range rows {
		if row == nil || row.Status != StatusActive {
			continue
		}
		if row.Held {
			return fmt.Errorf("%w: %s generation %d", ErrDualCancelHeld, row.TaskRef, row.Generation)
		}
		ids, err := recoveryHoldIdentities(row)
		if err != nil {
			continue
		}
		if !ok {
			return fmt.Errorf("%w: hold authority is required for %s", ErrDualCancelHeld, row.TaskRef)
		}
		if err := fencer.WithUnheldTransition(ctx, ids, func() error { return nil }); err != nil {
			if errors.Is(err, lifecycle.ErrHoldDenied) {
				return fmt.Errorf("%w: %v", ErrDualCancelHeld, err)
			}
			return err
		}
	}
	return nil
}

func releaseExactRow(ctx context.Context, store LeaseStore, holds lifecycle.HoldReader, row *Lease, now time.Time) error {
	if _, err := recoveryHoldIdentities(row); err == nil && holds != nil {
		mgr := NewClaimManager(store, WithHoldReader(holds), WithClock(func() time.Time { return now }))
		if err := mgr.Release(ctx, row.LeaseKey, row.OwnerID, row.Generation); err != nil {
			return err
		}
		return nil
	}
	_, _, err := store.Release(ctx, row.LeaseKey, row.OwnerID, row.Generation, now)
	return err
}

func exactReadback(ctx context.Context, snap dualSnapshotStore, key LeaseKey, generation int64) (*Lease, error) {
	rows, err := snap.LeasesByGeneration(ctx, key, generation)
	if err != nil {
		return nil, err
	}
	if len(rows) > 1 {
		return nil, fmt.Errorf("%w: %s generation %d", ErrDualCancelAmbiguousIdentity, key.TaskRef, generation)
	}
	return firstLease(rows), nil
}

func assertReleasedOrAbsent(row *Lease, expectedRelease bool) error {
	if row == nil {
		return nil
	}
	if row.Status == StatusReleased {
		if row.ReleasedAt == nil || row.ReleasedAt.IsZero() {
			return fmt.Errorf("released row missing durable timestamp")
		}
		return nil
	}
	if !expectedRelease && row.Status == StatusActive {
		return nil
	}
	return fmt.Errorf("readback status %s, want released", row.Status)
}

func reportFor(store, taskRef string, generation int64, before, after *Lease) StoreReport {
	rep := StoreReport{Store: store, TaskRef: taskRef, Generation: generation, Disposition: DispositionAbsent}
	if after == nil && before == nil {
		return rep
	}
	if after != nil && after.Status == StatusReleased {
		if before != nil && before.Status == StatusReleased {
			rep.Disposition = DispositionAlreadyReleased
			return rep
		}
		rep.Disposition = DispositionReleased
		return rep
	}
	if after != nil && after.Status == StatusActive {
		rep.Disposition = DispositionUnchanged
		return rep
	}
	if after == nil && before != nil && before.Status == StatusReleased {
		rep.Disposition = DispositionAlreadyReleased
	}
	return rep
}

func loadDualCancelJournal(path string, key LeaseKey, owner string, generation int64) (*dualCancelJournal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("%w: journal read: %v", ErrDualCancelPartialStore, err)
	}
	var last *dualCancelJournal
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec dualCancelJournal
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("%w: journal line %d malformed", ErrDualCancelPartialStore, i+1)
		}
		last = &rec
	}
	if last == nil || last.State == "done" {
		return nil, nil
	}
	if last.Repo != key.Repo || last.Provider != key.Provider || last.Project != key.Project || last.TaskRef != key.TaskRef || last.Owner != owner || last.Generation != generation {
		return nil, fmt.Errorf("%w: in-flight journal is for %s generation %d", ErrDualCancelAmbiguousIdentity, last.TaskRef, last.Generation)
	}
	return last, nil
}

func writeDualCancelJournal(path string, rec dualCancelJournal) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%w: journal path is required", ErrDualCancelPartialStore)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: journal directory: %v", ErrDualCancelPartialStore, err)
	}
	line, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("%w: journal open: %v", ErrDualCancelPartialStore, err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("%w: journal write: %v", ErrDualCancelPartialStore, err)
	}
	return f.Sync()
}
