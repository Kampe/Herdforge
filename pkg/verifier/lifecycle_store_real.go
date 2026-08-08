//go:build !fac151_hermetic_integration

package verifier

import (
	"context"

	"github.com/Kampe/Herdforge/pkg/containerlifecycle"
)

// realLifecycleStore is the production binding to the durable,
// SQLite-backed container lifecycle receipt store.
type realLifecycleStore struct {
	store *containerlifecycle.Store
}

// openLifecycleStore opens the durable receipt store at path.
func openLifecycleStore(path string) (lifecycleStore, error) {
	store, err := containerlifecycle.NewStore(path)
	if err != nil {
		return nil, err
	}
	return &realLifecycleStore{store: store}, nil
}

func (s *realLifecycleStore) Register(containerID, taskRef, generation, imageDigest, cleanupOwner string) error {
	_, err := s.store.Register(containerlifecycle.Receipt{
		ContainerID:  containerID,
		TaskRef:      taskRef,
		Generation:   generation,
		ImageDigest:  imageDigest,
		CleanupOwner: cleanupOwner,
	})
	return err
}

func (s *realLifecycleStore) MarkStarted(containerID string) error {
	return s.store.MarkStarted(containerID)
}

func (s *realLifecycleStore) EnsureCleanup(ctx context.Context, containerID, expectedTerminalState string, remove func(context.Context, string) error, absent func(context.Context, string) (bool, error)) error {
	return containerlifecycle.EnsureCleanup(ctx, s.store, containerID, expectedTerminalState, remove, absent)
}

func (s *realLifecycleStore) RemovedWithProvenAbsence(containerID string) bool {
	receipt, err := s.store.Get(containerID)
	if err != nil || receipt == nil {
		return false
	}
	return receipt.State == containerlifecycle.StateRemoved && receipt.AbsenceProved
}

func (s *realLifecycleStore) Close() error { return s.store.Close() }
