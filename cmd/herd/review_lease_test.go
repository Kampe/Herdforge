package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/dispatch"
)

func TestRequireLiveLeaseReacquiresReviewLeaseAfterWorkerRelease(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}
	store, err := claim.NewSQLiteLeaseStore(filepath.Join(root, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	key := claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: "FAC-1723"}
	worker, err := store.Acquire(context.Background(), key, "coordinator-worker", dispatch.RoleWorker, "", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Release(context.Background(), key, worker.OwnerID, worker.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	tc := dispatch.TaskContext{
		ProviderType: "memory", ProjectID: "project", Repository: "repo", Role: dispatch.RoleWorker,
		TaskRef: "FAC-1723", TaskID: "task-1723", Branch: "worker", BaseSHA: "base",
		LeaseID: fmt.Sprintf("claim:%d", worker.ID), LeaseGeneration: worker.Generation,
		LeaseTaskRef: "FAC-1723", SessionID: "session", AllowedOps: dispatch.WorkerOps,
		ExpiresAt: time.Now().Add(time.Hour),
	}
	if err := requireLiveLease(context.Background(), root, tc); err != nil {
		t.Fatal(err)
	}
	review, err := store.CurrentLease(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "memory", Project: "project", TaskRef: reviewLeaseTaskRef("FAC-1723")})
	if err != nil {
		t.Fatal(err)
	}
	if review == nil || review.Status != claim.StatusActive || review.Role != dispatch.RoleWorker {
		t.Fatalf("review lease not active: %+v", review)
	}
}
