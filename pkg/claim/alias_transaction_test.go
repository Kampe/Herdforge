package claim

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestAcquireFromAliasesSerializesAgainstOrdinaryAliasAcquire(t *testing.T) {
	store, err := NewSQLiteLeaseStore(filepath.Join(t.TempDir(), "leases.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	keyA := LeaseKey{Repo: "registered-a/opaque-repository", Provider: "memory", Project: "proj", TaskRef: "FAC-782:review"}
	keyB := LeaseKey{Repo: "registered-b/opaque-repository", Provider: "memory", Project: "proj", TaskRef: "FAC-782:review"}
	ctx := context.Background()
	inspection := make(chan struct{})
	release := make(chan struct{})
	previousHook := aliasAcquireBeforeInsertHook
	aliasAcquireBeforeInsertHook = func() {
		close(inspection)
		<-release
	}
	defer func() { aliasAcquireBeforeInsertHook = previousHook }()

	aliasResult := make(chan struct {
		lease *Lease
		err   error
	}, 1)
	go func() {
		lease, err := store.AcquireFromAliases(ctx, []LeaseKey{keyA, keyB}, keyA, "approval-owner", "worker", "", "repo", "worker", "lane", time.Now(), time.Hour)
		aliasResult <- struct {
			lease *Lease
			err   error
		}{lease, err}
	}()
	<-inspection

	competitorResult := make(chan error, 1)
	go func() {
		_, err := store.Acquire(ctx, keyB, "legacy-owner", "worker", "", time.Now(), time.Hour)
		competitorResult <- err
	}()
	select {
	case err := <-competitorResult:
		t.Fatalf("ordinary alias acquisition escaped the held claim transaction: %v", err)
	default:
	}
	close(release)

	result := <-aliasResult
	if result.err != nil {
		t.Fatalf("alias acquisition: %v", result.err)
	}
	if result.lease == nil || result.lease.LeaseKey != keyA || result.lease.Generation != 1 {
		t.Fatalf("alias lease=%+v, want key A generation 1", result.lease)
	}
	if err := <-competitorResult; err == nil {
		t.Fatal("legacy alias owner was admitted after the alias transaction committed")
	}
	if current, err := store.CurrentLease(ctx, keyB); err != nil {
		t.Fatal(err)
	} else if current != nil {
		t.Fatalf("foreign alias acquired a durable owner after approval: %+v", current)
	}
}
