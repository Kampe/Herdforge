package deps

import "context"

// ExactCardGraphSnapshot loads the relation graph needed to admit ONE card.
//
// Prefer SnapshotGraphForTask when the store exposes it. A whole-project
// SnapshotGraph is what made `herd deps check <ref>` time out on Kaneo: the
// exact-card vacuity guard (RejectEmptyProviderGraph) was paid for with a
// project-wide relation fetch before RequireTaskLaunch, which already knows
// how to scope. FAC-707 fixed the MESSAGE for that timeout and left the fetch.
//
// Fallback to SnapshotGraph only when the store has no scoped surface, so older
// adapters keep working without a FAST/bypass mode.
func ExactCardGraphSnapshot(ctx context.Context, store RelationStore, taskRef Ref, taskID TaskID, desiredEdges []DependencyEdge) (*GraphSnapshot, error) {
	if scoped, ok := store.(interface {
		SnapshotGraphForTask(context.Context, Ref, TaskID, []DependencyEdge) (*GraphSnapshot, error)
	}); ok {
		return scoped.SnapshotGraphForTask(ctx, taskRef, taskID, desiredEdges)
	}
	return store.SnapshotGraph(ctx)
}
