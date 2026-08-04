package scopefence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
)

func TestSQLiteStorePersistsExactOwnershipAndEvidenceAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scopefence.db")
	owner := req("FAC-200", "durable", 4, scope("pkg/durable", "pkg/durable.go", "Run")).Ownership
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if won, err := store.CompareAndSwap(context.Background(), "1", []Ownership{owner}); err != nil || !won {
		t.Fatalf("initial CAS: won=%v err=%v", won, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	snapshot, err := restarted.Read(context.Background())
	if err != nil || snapshot.Revision != "2" || len(snapshot.Owners) != 1 || snapshot.Owners[0].Identity != owner.Identity || snapshot.Owners[0].State != owner.State || !scopesEqual(snapshot.Owners[0].Scope, owner.Scope) {
		t.Fatalf("restart lost exact ownership: snapshot=%+v err=%v", snapshot, err)
	}
	evidence, err := restarted.ReadEvidence(context.Background())
	if err != nil || len(evidence) != 1 || evidence[0].Repository != owner.Repository || evidence[0].Task != owner.Task || evidence[0].Generation != owner.Generation || evidence[0].GraphRevision != owner.GraphRevision || evidence[0].GraphFiles != owner.GraphFiles || evidence[0].Reason != persistedEvidenceReason {
		t.Fatalf("restart lost exact evidence: evidence=%+v err=%v", evidence, err)
	}
}

func TestSQLiteStoreTwoIndependentHandlesCASExactlyOneWinner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scopefence.db")
	storeA, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	owners := []Ownership{
		req("FAC-201", "a", 1, scope("pkg/race", "pkg/race.go", "A")).Ownership,
		req("FAC-202", "b", 1, scope("pkg/race", "pkg/race.go", "B")).Ownership,
	}
	results := make(chan bool, 2)
	var wg sync.WaitGroup
	for i, store := range []*SQLiteStore{storeA, storeB} {
		wg.Add(1)
		go func(index int, store *SQLiteStore, owner Ownership) {
			defer wg.Done()
			won, err := store.CompareAndSwap(context.Background(), "1", []Ownership{owner})
			if err != nil {
				t.Errorf("CAS handle %d: %v", index, err)
			}
			results <- won
		}(i, store, owners[i])
	}
	wg.Wait()
	close(results)
	wins := 0
	for won := range results {
		if won {
			wins++
		}
	}
	if wins != 1 {
		t.Fatalf("expected one CAS winner, got %d", wins)
	}
}

func TestSQLiteGraphAuthorityBindsRepositoryRevisionAndCompleteness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scopefence.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	graph := Graph{Revision: "graph-200", Nodes: 20, Edges: 30, Files: 4, Flows: 2, Complete: true}
	if err := store.PutGraphSnapshot(context.Background(), "repo", graph); err != nil {
		t.Fatal(err)
	}
	authority := NewSQLiteGraphAuthority(store, "repo", graph.Revision, graph.Files)
	trusted, err := authority.Current(context.Background())
	if err != nil || trusted.Snapshot != graph || trusted.ExpectedRevision != graph.Revision || trusted.ExpectedFiles != graph.Files {
		t.Fatalf("unexpected trusted graph: %+v err=%v", trusted, err)
	}
	if _, err := NewSQLiteGraphAuthority(store, "other", graph.Revision, graph.Files).Current(context.Background()); err == nil {
		t.Fatal("repository mismatch was trusted")
	}
	if err := store.PutGraphSnapshot(context.Background(), "incomplete", Graph{Revision: "bad", Nodes: 1, Edges: 1, Files: 1, Flows: 0}); err == nil {
		t.Fatal("incomplete graph snapshot was persisted")
	}
	if _, err := NewSQLiteGraphAuthority(store, "repo", "wrong", graph.Files).Current(context.Background()); err == nil {
		t.Fatal("revision mismatch was trusted")
	}
}

func TestSQLiteStorePersistsDeterministicReleaseProofAndRejectsStaleGeneration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scopefence.db")
	owner := req("FAC-203", "release", 7, scope("pkg/release", "pkg/release.go", "Run")).Ownership
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if won, err := store.CompareAndSwap(context.Background(), "1", []Ownership{owner}); err != nil || !won {
		t.Fatalf("seed ownership: won=%v err=%v", won, err)
	}
	f := Fence{Store: store, Verify: func(_ context.Context, r ReleaseRequest) bool { return r.Proof == "root-proof" }}
	stale := ReleaseRequest{Ownership: owner, Authority: RootAdmittedMerge, Proof: "root-proof"}
	stale.Generation--
	if err := f.Release(context.Background(), stale); err == nil {
		t.Fatal("stale generation release succeeded")
	}
	if _, err := store.ReadReleaseProof(context.Background(), stale); err == nil {
		t.Fatal("stale generation release persisted proof")
	}

	release := ReleaseRequest{Ownership: owner, Authority: RootAdmittedMerge, Proof: "root-proof"}
	if err := f.Release(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	record, err := store.ReadReleaseProof(context.Background(), release)
	if err != nil || !reflect.DeepEqual(record.Ownership, owner) || record.Authority != RootAdmittedMerge {
		t.Fatalf("release proof missing exact binding: %+v err=%v", record, err)
	}
	digest := sha256.Sum256([]byte(release.Proof))
	if record.ProofDigest != hex.EncodeToString(digest[:]) || record.Key != ReleaseProofKey(release) {
		t.Fatalf("release proof was not deterministic: %+v", record)
	}
	if err := store.RecordReleaseProof(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	again, err := store.ReadReleaseProof(context.Background(), release)
	if err != nil || !reflect.DeepEqual(again, record) {
		t.Fatalf("replaying proof changed durable record: %+v err=%v", again, err)
	}
}

func TestSQLiteStoreRejectsInvalidCASOwnerWithoutChangingRevision(t *testing.T) {
	path := filepath.Join(t.TempDir(), "scopefence.db")
	store, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	invalid := req("FAC-204", "invalid", 1, scope("pkg/invalid", "pkg/invalid.go", "Run")).Ownership
	invalid.State = State("unknown")
	if won, err := store.CompareAndSwap(context.Background(), "1", []Ownership{invalid}); err == nil || won {
		t.Fatalf("invalid owner CAS was accepted: won=%v err=%v", won, err)
	}
	snapshot, err := store.Read(context.Background())
	if err != nil || snapshot.Revision != "1" || len(snapshot.Owners) != 0 {
		t.Fatalf("invalid CAS changed durable state: %+v err=%v", snapshot, err)
	}
}

func TestSQLiteStoreRejectsNoncanonicalRevisionBeforeSQLiteCoercion(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := req("FAC-208", "strict", 1, scope("pkg/strict", "pkg/strict.go", "Run")).Ownership
	for _, revision := range []string{"", "01", "1.0", "+1", " 1"} {
		if won, err := store.CompareAndSwap(context.Background(), revision, []Ownership{owner}); err == nil || won {
			t.Fatalf("revision %q was accepted: won=%v err=%v", revision, won, err)
		}
	}
}

func TestSQLiteStoreAtomicReleaseRollsBackWhenProofWriteFails(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := req("FAC-209", "atomic", 1, scope("pkg/atomic", "pkg/atomic.go", "Run")).Ownership
	if won, err := store.CompareAndSwap(context.Background(), "1", []Ownership{owner}); err != nil || !won {
		t.Fatalf("seed: %v", err)
	}
	store.proofWriteHook = func() error { return errors.New("forced proof write failure") }
	f := Fence{Store: store, Verify: func(_ context.Context, r ReleaseRequest) bool { return r.Proof == ReleaseProof(r) }}
	release := ReleaseRequest{Ownership: owner, Authority: CompensatedNoCandidate}
	release.Proof = ReleaseProof(release)
	if err := f.Release(context.Background(), release); err == nil {
		t.Fatal("forced proof failure was swallowed")
	}
	snapshot, err := store.Read(context.Background())
	if err != nil || len(snapshot.Owners) != 1 || snapshot.Owners[0].Generation != owner.Generation {
		t.Fatalf("atomic release removed owner before proof: %+v err=%v", snapshot, err)
	}
}

func TestSQLiteStoreConflictingProofReplayFailsClosed(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := req("FAC-210", "proof", 1, scope("pkg/proof", "pkg/proof.go", "Run")).Ownership
	r := ReleaseRequest{Ownership: owner, Authority: RootAdmittedMerge, Proof: "first"}
	if err := store.RecordReleaseProof(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	conflict := r
	conflict.Proof = "second"
	if err := store.RecordReleaseProof(context.Background(), conflict); err == nil {
		t.Fatal("conflicting proof replay was accepted")
	}
	record, err := store.ReadReleaseProof(context.Background(), r)
	if err != nil || record.ProofDigest == "" {
		t.Fatalf("first proof was not preserved: %+v err=%v", record, err)
	}
}

func TestSQLiteStoreReadRejectsEvidenceBindingCorruption(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	owner := req("FAC-211", "evidence", 1, scope("pkg/evidence", "pkg/evidence.go", "Run")).Ownership
	if won, err := store.CompareAndSwap(context.Background(), "1", []Ownership{owner}); err != nil || !won {
		t.Fatalf("seed: %v", err)
	}
	if _, err := store.db.Exec(`UPDATE scopefence_owners SET evidence_json = '{"reason":"tampered"}'`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Read(context.Background()); err == nil {
		t.Fatal("corrupt evidence was returned as trusted")
	}
}

func TestSQLiteScopeAuthorityRequiresExactGraphRevisionAndCanonicalScope(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	declared := Scope{Packages: []string{"pkg/authority"}}
	if err := store.PutScopeDeclaration(context.Background(), "repo", "FAC-212", "graph-1", declared); err != nil {
		t.Fatal(err)
	}
	authority := NewSQLiteScopeAuthority(store)
	resolved, err := authority.Resolve(context.Background(), "repo", "FAC-212", "graph-1")
	if err != nil || !scopesEqual(resolved, declared) {
		t.Fatalf("scope resolution failed: %+v err=%v", resolved, err)
	}
	if _, err := authority.Resolve(context.Background(), "repo", "FAC-212", "stale"); err == nil {
		t.Fatal("stale scope declaration was trusted")
	}
	if err := store.PutScopeDeclaration(context.Background(), "repo", "FAC-213", "graph-1", Scope{Packages: []string{"pkg//raw"}}); err == nil {
		t.Fatal("noncanonical scope declaration was persisted")
	}
}

func TestResolvingFenceIgnoresCallerScopeAndUsesRevisionBoundAuthority(t *testing.T) {
	store, err := NewSQLiteStore(filepath.Join(t.TempDir(), "scopefence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	graph := Graph{Revision: "graph-2", Nodes: 10, Edges: 20, Files: 2, Flows: 1, Complete: true}
	if err := store.PutGraphSnapshot(context.Background(), "repo", graph); err != nil {
		t.Fatal(err)
	}
	trusted := Scope{Packages: []string{"pkg/trusted"}}
	if err := store.PutScopeDeclaration(context.Background(), "repo", "FAC-214", graph.Revision, trusted); err != nil {
		t.Fatal(err)
	}
	resolving := ResolvingFence{
		Fence:     Fence{Store: store, Graph: NewSQLiteGraphAuthority(store, "repo", graph.Revision, graph.Files)},
		Authority: NewSQLiteScopeAuthority(store),
	}
	request := req("FAC-214", "branch", 1, Scope{Packages: []string{"pkg/underdeclared"}})
	request.Repository, request.Task = "repo", "FAC-214"
	request.ExpectedGraphRevision = graph.Revision
	decision, err := resolving.Acquire(context.Background(), request)
	if err != nil || !decision.Granted || decision.Lease == nil || !scopesEqual(decision.Lease.Scope, trusted) {
		t.Fatalf("caller scope bypassed authority: decision=%+v err=%v", decision, err)
	}
}
