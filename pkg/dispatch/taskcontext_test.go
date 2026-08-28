package dispatch

import (
	"encoding/json"
	"github.com/Kampe/Herdforge/pkg/provider"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func validTaskContext() TaskContext {
	return TaskContext{
		ProviderType:      "kaneo",
		ProjectID:         "proj-x",
		ProviderWorkspace: "ws-1",
		ProviderProfile:   "KANEO_API_KEY",
		Repository:        "herdforge",
		Role:              RoleWorker,
		TaskRef:           "FAC-145",
		TaskID:            "task-id-1",
		Branch:            "herd/fac-145",
		BaseSHA:           "abc123",
		LeaseID:           "lease-1",
		LeaseGeneration:   3,
		LeaseTaskRef:      "FAC-145",
		SessionID:         "worker-testsession",
		AllowedOps:        WorkerOps,
		// UTC + Truncate strips the monotonic clock so a JSON round trip
		// compares equal with reflect.DeepEqual.
		ExpiresAt: time.Now().UTC().Add(time.Hour).Truncate(time.Second),
	}
}

func TestTaskContextValidate_FailsClosedOnEveryRequiredField(t *testing.T) {
	if err := validTaskContext().Validate(); err != nil {
		t.Fatalf("complete context must validate: %v", err)
	}
	// FAC-145: any blank required field is exactly the NULL-project /
	// unattributable-mutation / unfenced-authority class the receipt exists
	// to prevent.
	blank := map[string]func(*TaskContext){
		"provider_type":    func(tc *TaskContext) { tc.ProviderType = " " },
		"project_id":       func(tc *TaskContext) { tc.ProjectID = "" },
		"repository":       func(tc *TaskContext) { tc.Repository = "" },
		"role":             func(tc *TaskContext) { tc.Role = " " },
		"task_ref":         func(tc *TaskContext) { tc.TaskRef = "" },
		"task_id":          func(tc *TaskContext) { tc.TaskID = "\t" },
		"branch":           func(tc *TaskContext) { tc.Branch = "" },
		"base_sha":         func(tc *TaskContext) { tc.BaseSHA = "" },
		"lease_id":         func(tc *TaskContext) { tc.LeaseID = "" },
		"lease_task_ref":   func(tc *TaskContext) { tc.LeaseTaskRef = "" },
		"lease_generation": func(tc *TaskContext) { tc.LeaseGeneration = 0 },
		"allowed_ops":      func(tc *TaskContext) { tc.AllowedOps = nil },
		"expires_at":       func(tc *TaskContext) { tc.ExpiresAt = time.Time{} },
	}
	for field, mutate := range blank {
		tc := validTaskContext()
		mutate(&tc)
		err := tc.Validate()
		if err == nil {
			t.Errorf("blank %s must fail closed", field)
			continue
		}
		if !strings.Contains(err.Error(), field) {
			t.Errorf("error for blank %s must name the field, got: %v", field, err)
		}
	}
}

// FAC-145 role policy: unknown roles and role-op mismatches are invalid at
// the root — an over-privileged or unclassifiable receipt never validates.
func TestTaskContextValidate_RolePolicy(t *testing.T) {
	unknown := validTaskContext()
	unknown.Role = "integrator"
	if err := unknown.Validate(); err == nil {
		t.Fatal("unknown role must carry no authority")
	}

	over := validTaskContext()
	over.AllowedOps = CoordinatorOps // worker + mutate
	if err := over.Validate(); err == nil {
		t.Fatal("worker receipt carrying mutate must be invalid")
	}
	overReviewer := validTaskContext()
	overReviewer.Role = RoleReviewer
	overReviewer.AllowedOps = CoordinatorOps
	if err := overReviewer.Validate(); err == nil {
		t.Fatal("reviewer receipt carrying mutate must be invalid")
	}

	coord := validTaskContext()
	coord.Role = RoleCoordinator
	coord.AllowedOps = CoordinatorOps
	if err := coord.Validate(); err != nil {
		t.Fatalf("coordinator with mutate must validate: %v", err)
	}
}

// The receipt is the SOLE file written — no provider-native context file is
// seeded (that was an ambient-mutation affordance), and the write is
// atomic: a failed commit leaves no partial receipt and no temp litter.
func TestWriteTaskContext_SingleAuthorityFile(t *testing.T) {
	dir := t.TempDir()
	tc := validTaskContext()
	if err := WriteTaskContext(dir, tc); err != nil {
		t.Fatalf("WriteTaskContext: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != TaskContextFile {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("exactly one authority file expected, got %v", names)
	}

	var got TaskContext
	data, err := os.ReadFile(filepath.Join(dir, TaskContextFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("receipt not valid JSON: %v", err)
	}
	if !reflect.DeepEqual(got, tc) {
		t.Errorf("receipt round-trip mismatch:\n got %+v\nwant %+v", got, tc)
	}
}

// A failed commit (target blocked) must surface an error, keep no partial
// state, and leave no temp litter behind.
func TestWriteTaskContext_FailedCommitLeavesNoPartialState(t *testing.T) {
	dir := t.TempDir()
	// A non-empty directory at the receipt path makes the rename fail.
	if err := os.MkdirAll(filepath.Join(dir, TaskContextFile, "block"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := WriteTaskContext(dir, validTaskContext()); err == nil {
		t.Fatal("commit failure must surface as an error")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("temp litter left behind: %s", e.Name())
		}
	}
}

// Re-issuing over an existing receipt replaces it atomically — the old
// receipt stays intact until the single rename commits the new one.
func TestWriteTaskContext_ReissueReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	first := validTaskContext()
	if err := WriteTaskContext(dir, first); err != nil {
		t.Fatal(err)
	}
	second := validTaskContext()
	second.HerdrWorkspace = "wHerd"
	if err := WriteTaskContext(dir, second); err != nil {
		t.Fatal(err)
	}
	got, err := ReadTaskContext(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.HerdrWorkspace != "wHerd" {
		t.Fatalf("re-issue did not land: %+v", got)
	}
}

// FAC-145: untrusted refs can never traverse out of the canonical store,
// and reviewer receipts without an exact candidate are invalid.
func TestCanonicalReceipt_TraversalAndReviewerCandidate(t *testing.T) {
	signer, _ := testSignerVerifier(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}

	evil := validTaskContext()
	evil.TaskRef = "../evil"
	signedEvil, err := signer.Issue(evil)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signedEvil); err == nil {
		t.Fatal("traversal-capable ref must be refused by the store")
	}
	if _, err := LoadCanonicalReceipt(root, "../evil"); err == nil {
		t.Fatal("traversal-capable ref must be refused by the loader")
	}

	rev := validTaskContext()
	rev.Role = RoleReviewer
	rev.AllowedOps = ReviewerOps
	rev.CandidateSHA = ""
	if err := rev.Validate(); err == nil {
		t.Fatal("reviewer receipt without candidate_sha must be invalid")
	}
	rev.CandidateSHA = "cafe1234"
	if err := rev.Validate(); err != nil {
		t.Fatalf("reviewer receipt with candidate must validate: %v", err)
	}
}

// FAC-629: dispatch authorization and landing evidence have different
// schemas and must never share a loader or directory. A landing receipt at
// the completion path is invisible to the task-context loader by design.
func TestCanonicalTaskContextNamespaceDoesNotLoadLandingReceipt(t *testing.T) {
	root := t.TempDir()
	landingDir := filepath.Join(root, ".herd", "receipts")
	if err := os.MkdirAll(landingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	landingPath := filepath.Join(landingDir, "FAC-145.json")
	if err := os.WriteFile(landingPath, []byte(`{"task_ref":"FAC-145","candidate_sha":"abc","merge_sha":"def","verdict":"PASS"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadCanonicalReceipt(root, "FAC-145")
	if err == nil {
		t.Fatal("landing receipt must not load as dispatch task context")
	}
	if !strings.Contains(err.Error(), CanonicalTaskContextDir) {
		t.Fatalf("task-context loader must name its own namespace, got %v", err)
	}

	signer, _ := testSignerVerifier(t)
	signed, err := signer.Issue(validTaskContext())
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signed); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(root, CanonicalTaskContextDir))
	if err != nil || len(entries) == 0 {
		t.Fatalf("task context was not stored in %s: entries=%d err=%v", CanonicalTaskContextDir, len(entries), err)
	}
}

// FAC-145: the canonical store is MONOTONIC — a delayed older-generation
// writer can never roll authority back — and every commit is read back and
// must round-trip exactly.
func TestCanonicalReceipt_MonotonicNoRollback(t *testing.T) {
	signer, _ := testSignerVerifier(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".herd"), 0755); err != nil {
		t.Fatal(err)
	}

	gen2 := validTaskContext()
	gen2.LeaseGeneration = 2
	signed2, err := signer.Issue(gen2)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signed2); err != nil {
		t.Fatalf("store gen2: %v", err)
	}

	gen1 := validTaskContext()
	gen1.LeaseGeneration = 1
	signed1, err := signer.Issue(gen1)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signed1); err == nil {
		t.Fatal("older generation must not overwrite newer canonical authority")
	}
	back, err := LoadCanonicalReceipt(root, gen2.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	if back.LeaseGeneration != 2 {
		t.Fatalf("canonical authority rolled back to generation %d", back.LeaseGeneration)
	}

	// Same-generation transitions are IMMUTABLE-field-preserving: only the
	// workspace stamp is sanctioned.
	gen2b := gen2
	gen2b.HerdrWorkspace = "wHerd"
	signed2b, err := signer.Issue(gen2b)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signed2b); err != nil {
		t.Fatalf("workspace stamp must land: %v", err)
	}

	// A delayed old same-generation writer with ANY other change is refused:
	// different expiry, different candidate, different workspace.
	oldWriter := gen2
	oldWriter.ExpiresAt = gen2.ExpiresAt.Add(48 * time.Hour)
	signedOld, err := signer.Issue(oldWriter)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signedOld); err == nil {
		t.Fatal("same-generation expiry change must be refused")
	}
	otherWS := gen2b
	otherWS.HerdrWorkspace = "wOther"
	signedWS, err := signer.Issue(otherWS)
	if err != nil {
		t.Fatal(err)
	}
	if err := StoreCanonicalReceipt(root, signedWS); err == nil {
		t.Fatal("workspace re-stamp to a different value must be refused")
	}
	back2, err := LoadCanonicalReceipt(root, gen2.TaskRef)
	if err != nil {
		t.Fatal(err)
	}
	if back2.HerdrWorkspace != "wHerd" {
		t.Fatalf("canonical authority mutated by refused writer: %+v", back2)
	}
}

func TestWriteTaskContext_InvalidContextWritesNothing(t *testing.T) {
	dir := t.TempDir()
	tc := validTaskContext()
	tc.ProjectID = ""
	if err := WriteTaskContext(dir, tc); err == nil {
		t.Fatal("invalid context must fail closed")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("failed validation must leave no files, found %d", len(entries))
	}
}

// FAC-680: an expired receipt is not a fault, it is the TTL doing its job -- a
// day covers any legitimate build without leaving immortal mutation authority in
// an abandoned worktree. Observed live: "receipt FAC-548 expired at 2026-08-22"
// refused a board transition four days later, correctly.
//
// What was missing was the remedy. The refusal named the expiry and nothing
// else, so a lane hitting it could only report being stuck -- the same dead end
// the unrecorded-provenance gate was before it became operator-decidable.
func TestExpiredReceiptAuthorizesNothingAndSaysWhen(t *testing.T) {
	tc := TaskContext{
		TaskRef:    "FAC-548",
		Role:       "reviewer",
		AllowedOps: []string{"update_status"},
		ExpiresAt:  time.Now().Add(-96 * time.Hour),
	}
	err := tc.Authorize(time.Now(), provider.OpKind("update_status"))
	if err == nil {
		t.Fatal("an expired receipt must authorize nothing, however well-formed")
	}
	if !strings.Contains(err.Error(), "expired") {
		t.Errorf("the refusal must name expiry as the cause: %v", err)
	}
	// A receipt inside its window still authorizes its own ops, or the TTL would
	// simply break every legitimate build.
	tc.ExpiresAt = time.Now().Add(time.Hour)
	if err := tc.Authorize(time.Now(), provider.OpKind("update_status")); err != nil {
		t.Errorf("a live receipt must still authorize its allowed op: %v", err)
	}
	// And it never authorizes an op outside its grant, expired or not.
	if err := tc.Authorize(time.Now(), provider.OpKind("delete_task")); err == nil {
		t.Error("a live receipt must not authorize an op outside its grant")
	}
}

// The TTL is a day on purpose: long enough for any real build, short enough that
// an abandoned worktree cannot mutate the board indefinitely.
func TestReceiptTTLIsBoundedToADay(t *testing.T) {
	if DefaultReceiptTTL != 24*time.Hour {
		t.Fatalf("TTL = %v; a day is the balance between a long build and an immortal authority", DefaultReceiptTTL)
	}
}
