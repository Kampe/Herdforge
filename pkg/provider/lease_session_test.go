package provider

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"golang.org/x/sys/unix"
)

func testCapability(owner string, gen int64) LeaseCapability {
	ch, _ := MintChallenge()
	return LeaseCapability{
		OwnerID:    owner,
		Generation: gen,
		TaskRef:    "FAC-HB",
		Repo:       "/tmp/repo",
		Provider:   "memory",
		Project:    "p",
		Receipt:    "receipt-final-1",
		Challenge:  ch,
		ExpiresAt:  time.Now().UTC().Add(time.Hour),
	}
}

func TestLeaseSession_WriteRead_ONoFollow(t *testing.T) {
	wt := t.TempDir()
	cap := testCapability("owner-a", 3)
	if err := WriteLeaseCapability(wt, cap); err != nil {
		t.Fatal(err)
	}
	got, err := ReadLeaseCapability(wt)
	if err != nil {
		t.Fatal(err)
	}
	if got.OwnerID != "owner-a" || got.Generation != 3 {
		t.Fatalf("got %+v", got)
	}
}

func TestLeaseSession_RefuseCapabilitySymlink(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(wt, "evil.json")
	if err := os.WriteFile(target, []byte(`{"owner_id":"x","generation":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(wt, CapabilityFileName)
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err := ReadLeaseCapability(wt)
	if err == nil {
		t.Fatal("must refuse symlink capability")
	}
	if err != unix.ELOOP && !strings.Contains(err.Error(), "symlink") {
		// ELOOP from O_NOFOLLOW is acceptable
		if !strings.Contains(err.Error(), "capability") && err != unix.ELOOP {
			t.Logf("err=%v (acceptable fail-closed)", err)
		}
	}
}

func TestLeaseSession_RefuseHerdDirSymlink(t *testing.T) {
	wt := t.TempDir()
	evil := t.TempDir()
	if err := os.Symlink(evil, filepath.Join(wt, ".herd")); err != nil {
		t.Fatal(err)
	}
	err := WriteLeaseCapability(wt, testCapability("o", 1))
	if err == nil || (!strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "not a directory")) {
		t.Fatalf("want .herd symlink refuse, got %v", err)
	}
}

func TestLeaseSession_HandbackDone_GenerationBound(t *testing.T) {
	wt := t.TempDir()
	// Legacy plain-text done must not satisfy generation-bound match.
	if err := os.MkdirAll(filepath.Join(wt, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, HandbackDoneName), []byte("owner=o gen=1 at=now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HandbackDoneMatches(wt, "o", 1) {
		t.Fatal("legacy plain-text done must not match")
	}
	_, err := ReadHandbackDone(wt)
	if err == nil {
		t.Fatal("legacy done must error")
	}

	// Valid JSON receipt.
	if err := WriteHandbackDone(wt, HandbackDoneReceipt{
		OwnerID: "o", Generation: 2, TaskRef: "FAC-HB", Receipt: "r", Challenge: "c",
	}); err != nil {
		t.Fatal(err)
	}
	if !HandbackDoneMatches(wt, "o", 2) {
		t.Fatal("want gen-bound match")
	}
	if HandbackDoneMatches(wt, "o", 1) {
		t.Fatal("wrong gen must not match")
	}
	if HandbackDoneMatches(wt, "other", 2) {
		t.Fatal("wrong owner must not match")
	}
}

func TestLeaseSession_NewCapabilityClearsStaleDone(t *testing.T) {
	wt := t.TempDir()
	if err := WriteHandbackDone(wt, HandbackDoneReceipt{
		OwnerID: "old", Generation: 1, TaskRef: "FAC-HB", Receipt: "r", Challenge: "c",
	}); err != nil {
		t.Fatal(err)
	}
	if err := WriteLeaseCapability(wt, testCapability("new", 2)); err != nil {
		t.Fatal(err)
	}
	if HandbackDoneMatches(wt, "old", 1) {
		t.Fatal("stale done must be cleared on new capability")
	}
}

func TestLeaseSession_TryAutoHandback_IdempotentAcrossRelease(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp := NewMemoryProvider()
	stack, err := OpenClaimStack(filepath.Join(dir, "claim"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	key := claim.LeaseKey{Repo: dir, Provider: "memory", Project: "p", TaskRef: "FAC-HB"}
	lease, err := stack.AcquireLease(ctx, key, "worker-1", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	ch, _ := MintChallenge()
	cap := LeaseCapability{
		OwnerID: lease.OwnerID, Generation: lease.Generation,
		TaskRef: key.TaskRef, Repo: key.Repo, Provider: key.Provider, Project: key.Project,
		Receipt: "rcpt-1", Challenge: ch, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := WriteLeaseCapability(wt, cap); err != nil {
		t.Fatal(err)
	}

	// First handback: full path.
	if err := TryAutoHandback(ctx, stack.Manager, wt); err != nil {
		t.Fatalf("handback 1: %v", err)
	}
	if present, err := CapabilityPresent(wt); err != nil || present {
		t.Fatalf("capability must be gone: present=%v err=%v", present, err)
	}
	if !HandbackDoneMatches(wt, lease.OwnerID, lease.Generation) {
		t.Fatal("done receipt must match generation")
	}
	// Second handback: idempotent (already done, no capability).
	if err := TryAutoHandback(ctx, stack.Manager, wt); err != nil {
		t.Fatalf("handback 2 idempotent: %v", err)
	}
}

func TestLeaseSession_TryAutoHandback_CrashAfterRemoteBeforeDone(t *testing.T) {
	// Simulate: intent written + remote released, then crash before done.
	// Retry must complete local durability via isAlreadyReleased / RemoteReleased.
	ctx := context.Background()
	dir := t.TempDir()
	mp := NewMemoryProvider()
	stack, err := OpenClaimStack(filepath.Join(dir, "claim"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()

	key := claim.LeaseKey{Repo: dir, Provider: "memory", Project: "p", TaskRef: "FAC-CR"}
	lease, err := stack.AcquireLease(ctx, key, "worker-2", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	ch, _ := MintChallenge()
	cap := LeaseCapability{
		OwnerID: lease.OwnerID, Generation: lease.Generation,
		TaskRef: key.TaskRef, Repo: key.Repo, Provider: key.Provider, Project: key.Project,
		Receipt: "rcpt-2", Challenge: ch, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := WriteLeaseCapability(wt, cap); err != nil {
		t.Fatal(err)
	}
	// Intent with remote already released (crash window after Release).
	intent := HandbackIntent{
		OwnerID: lease.OwnerID, Generation: lease.Generation,
		TaskRef: key.TaskRef, Repo: key.Repo, Provider: key.Provider, Project: key.Project,
		Receipt: "rcpt-2", Challenge: ch, RemoteReleased: true, IntentAt: time.Now().UTC(),
	}
	if err := WriteHandbackIntent(wt, intent); err != nil {
		t.Fatal(err)
	}
	// Actually release remote so lease is gone.
	if err := stack.Manager.Release(ctx, key, lease.OwnerID, lease.Generation); err != nil {
		t.Fatal(err)
	}
	// Capability still present (crash before remove).
	if err := TryAutoHandback(ctx, stack.Manager, wt); err != nil {
		t.Fatalf("resume after remote release: %v", err)
	}
	if present, _ := CapabilityPresent(wt); present {
		t.Fatal("capability must be removed")
	}
	if !HandbackDoneMatches(wt, lease.OwnerID, lease.Generation) {
		t.Fatal("done must match")
	}
	// Retry Release path: no capability, done present → success.
	if err := TryAutoHandback(ctx, stack.Manager, wt); err != nil {
		t.Fatalf("final idempotent: %v", err)
	}
}

func TestLeaseSession_CorruptCapability_FailClosed(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, ".herd"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, CapabilityFileName), []byte("{not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadLeaseCapability(wt)
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want corrupt error, got %v", err)
	}
	present, perr := CapabilityPresent(wt)
	if present || perr == nil {
		t.Fatalf("CapabilityPresent must fail-closed on corrupt: present=%v err=%v", present, perr)
	}
	mgr := claim.NewClaimManager(claim.NewInMemoryLeaseStore())
	if err := TryAutoHandback(context.Background(), mgr, wt); err == nil {
		t.Fatal("TryAutoHandback must fail closed on corrupt capability")
	}
}

func TestRequireTaskRole_FailClosedOnMismatch(t *testing.T) {
	task := &Task{Labels: []string{"forge-smith", "worker"}}
	if _, err := RequireTaskRole(task, "reviewer"); err == nil {
		t.Fatal("must refuse role mismatch")
	}
	got, err := RequireTaskRole(task, "worker")
	if err != nil || got != "worker" {
		t.Fatalf("got %q err=%v", got, err)
	}
	// Unlabeled: allow role as-is.
	got, err = RequireTaskRole(&Task{}, "reviewer")
	if err != nil || got != "reviewer" {
		t.Fatalf("unlabeled: got %q err=%v", got, err)
	}
}

func TestOpenClaimStack_KaneoRequiresSharedWhenForced(t *testing.T) {
	t.Setenv("HERD_FENCE_FORCE_SHARED", "1")
	t.Setenv("HERD_FENCE_ALLOW_LOCAL", "")
	t.Setenv("HERD_CLAIM_DIR", "")
	t.Setenv("HERD_FENCE_VOLUME_ID", "")
	kp := NewKaneoProvider("http://127.0.0.1:9", "proj", false)
	dir := t.TempDir()
	_, err := OpenClaimStack(dir, kp)
	if err == nil {
		t.Fatal("want SHARED/CLAIM_DIR required error")
	}
	ProvisionSharedFenceForTest(t, dir)
	stack, err := OpenClaimStack(dir, kp)
	if err != nil {
		t.Fatalf("with provisioned SHARED: %v", err)
	}
	_ = stack.Close()
}

func TestSharedMarker_IndependentLocalDirRefusedEvenWithStolenVolumeID(t *testing.T) {
	dirA := t.TempDir()
	ProvisionSharedFenceForTest(t, dirA)
	volID := os.Getenv("HERD_FENCE_VOLUME_ID")

	// Independent dir B: plant SHARED with stolen volume_id and claim_dir string
	// matching B (simulates same configured path string on another host with
	// a local dir) — fences.db seal will not match A's store.
	dirB := t.TempDir()
	absB, _ := filepath.Abs(dirB)
	if err := os.MkdirAll(dirB, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a different fences.db (different inode).
	if err := os.WriteFile(filepath.Join(dirB, "fences.db"), []byte("not-a-real-db"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Copy volume_id and claim_dir pointing at B (host B's HERD_CLAIM_DIR=/same/path
	// would still open a different local file — we model that as dirB).
	body := "herd-shared-fence-v4\nvolume_id=" + volID + "\nclaim_dir=" + absB +
		"\nfences_dev=0\nfences_ino=0\n"
	if err := os.WriteFile(filepath.Join(dirB, "SHARED"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_CLAIM_DIR", absB)
	t.Setenv("HERD_FENCE_VOLUME_ID", volID)
	if err := ValidateSharedMarker(dirB); err == nil {
		t.Fatal("independent fences.db must fail seal even with copied volume_id")
	}
	// Real provisioned volume still validates under its env.
	absA, _ := filepath.Abs(dirA)
	t.Setenv("HERD_CLAIM_DIR", absA)
	if err := ValidateSharedMarker(dirA); err != nil {
		t.Fatalf("fleet volume: %v", err)
	}
}

func TestSharedMarker_CopiedVolumeIDWithFreshFencesDBFails(t *testing.T) {
	// Same configured path simulation: provision A, then replace fences.db
	// (as if independent local store at that path) — seal must fail.
	dir := t.TempDir()
	ProvisionSharedFenceForTest(t, dir)
	// Replace fences.db with a new file (new inode).
	_ = os.Remove(filepath.Join(dir, "fences.db"))
	if err := os.WriteFile(filepath.Join(dir, "fences.db"), []byte("fresh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSharedMarker(dir); err == nil {
		t.Fatal("replaced fences.db must fail identity seal")
	}
}

func TestSharedMarker_RefuseSecondProvisionWithoutRotate(t *testing.T) {
	dir := t.TempDir()
	ProvisionSharedFenceForTest(t, dir)
	volID := os.Getenv("HERD_FENCE_VOLUME_ID")
	// Second provision without rotate must refuse.
	t.Setenv("HERD_FENCE_PROVISION", "1")
	t.Setenv("HERD_FENCE_ROTATE", "")
	// Clear env seal so first-time-branch is not hit; existing seal refuses.
	t.Setenv("HERD_FENCE_VOLUME_ID", "")
	if err := WriteSharedMarker(dir); err == nil {
		t.Fatal("second provision without rotate must refuse")
	}
	// Rotate without matching current id must refuse.
	t.Setenv("HERD_FENCE_PROVISION", "")
	t.Setenv("HERD_FENCE_ROTATE", "1")
	t.Setenv("HERD_FENCE_VOLUME_ID", "wrong-id-not-matching-existing-seal-at-all")
	if err := WriteSharedMarker(dir); err == nil {
		t.Fatal("rotate with wrong volume id must refuse")
	}
	// Authorized rotate.
	t.Setenv("HERD_FENCE_VOLUME_ID", volID)
	if err := WriteSharedMarker(dir); err != nil {
		t.Fatalf("authorized rotate: %v", err)
	}
	// Rotate must mint a new DB seal (not equal to prior env seal).
	newSeal, err := readDBVolumeSeal(filepath.Join(dir, "fences.db"))
	if err != nil {
		t.Fatal(err)
	}
	if newSeal == volID {
		t.Fatal("rotate must mint new volume_seal in fences.db")
	}
	// SHARED must not carry the seal (mode-0644 secret leak).
	raw, _ := os.ReadFile(filepath.Join(dir, "SHARED"))
	if strings.Contains(string(raw), "volume_id=") || strings.Contains(string(raw), newSeal) {
		t.Fatal("SHARED must not embed volume seal")
	}
}

// TestSharedMarker_StolenVolumeIDCannotFirstProvisionIndependentStore: a host
// with a copied VOLUME_ID must not mint a second fences.db that validates.
func TestSharedMarker_StolenVolumeIDCannotFirstProvisionIndependentStore(t *testing.T) {
	dirA := t.TempDir()
	ProvisionSharedFenceForTest(t, dirA)
	stolen := os.Getenv("HERD_FENCE_VOLUME_ID")
	if stolen == "" {
		t.Fatal("expected minted seal in env")
	}

	dirB := t.TempDir()
	absB, _ := filepath.Abs(dirB)
	t.Setenv("HERD_CLAIM_DIR", absB)
	t.Setenv("HERD_FENCE_PROVISION", "1")
	t.Setenv("HERD_FENCE_ROTATE", "")
	t.Setenv("HERD_FENCE_VOLUME_ID", stolen) // stolen from fleet A
	t.Setenv("HERD_FENCE_PROVISION_TOKEN", stolen)
	if err := WriteSharedMarker(absB); err == nil {
		t.Fatal("first-time provision with pre-set stolen volume id must refuse")
	}
	// Even after clearing stolen id, a new independent store gets a DIFFERENT seal.
	t.Setenv("HERD_FENCE_VOLUME_ID", "")
	t.Setenv("HERD_FENCE_PROVISION_TOKEN", "")
	if err := WriteSharedMarker(absB); err != nil {
		t.Fatalf("clean first provision: %v", err)
	}
	sealB, err := readDBVolumeSeal(filepath.Join(absB, "fences.db"))
	if err != nil {
		t.Fatal(err)
	}
	if sealB == stolen {
		t.Fatal("independent provision must not reuse stolen seal value")
	}
	// Fleet A env does not validate store B.
	t.Setenv("HERD_FENCE_VOLUME_ID", stolen)
	if err := ValidateSharedMarker(absB); err == nil {
		t.Fatal("stolen fleet seal must not validate independent store B")
	}
}

func TestCanonicalClaimDir_HonorsHERD_CLAIM_DIR(t *testing.T) {
	shared := t.TempDir()
	t.Setenv("HERD_CLAIM_DIR", shared)
	got, err := CanonicalClaimDir(".", "")
	if err != nil {
		t.Fatal(err)
	}
	abs, _ := filepath.Abs(shared)
	if got != abs {
		t.Fatalf("CanonicalClaimDir=%q want HERD_CLAIM_DIR %q", got, abs)
	}
}

func TestLeaseSession_LostAuthorityWithoutCapability(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp := NewMemoryProvider()
	stack, err := OpenClaimStack(filepath.Join(dir, "claim"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	key := claim.LeaseKey{Repo: dir, Provider: "memory", Project: "p", TaskRef: "FAC-LOST"}
	lease, err := stack.AcquireLease(ctx, key, "w-lost", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	ch, _ := MintChallenge()
	cap := LeaseCapability{
		OwnerID: lease.OwnerID, Generation: lease.Generation,
		TaskRef: key.TaskRef, Repo: key.Repo, Provider: key.Provider, Project: key.Project,
		Receipt: "rcpt-lost", Challenge: ch, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := WriteLeaseCapability(wt, cap); err != nil {
		t.Fatal(err)
	}
	// Destroy capability only — session remains incomplete (lost authority).
	if err := RemoveLeaseCapability(wt); err != nil {
		t.Fatal(err)
	}
	// Handback must recover via session, not silent success.
	if err := TryAutoHandback(ctx, stack.Manager, wt); err != nil {
		t.Fatalf("recover lost authority: %v", err)
	}
	sess, err := ReadLaunchSession(wt)
	if err != nil || sess == nil || !sess.Completed {
		t.Fatalf("session must complete: %+v err=%v", sess, err)
	}
}

func TestTaskOwnershipRole_ForgeSmithReview(t *testing.T) {
	task := &Task{Labels: []string{"forge-smith"}}
	got, err := TaskOwnershipRole(task, "reviewer")
	if err != nil || got != "forge-smith" {
		t.Fatalf("review session on forge-smith card: got %q err=%v", got, err)
	}
	// Unknown sole label refuses (not an implementation role).
	task2 := &Task{Labels: []string{"alpha"}}
	if _, err := TaskOwnershipRole(task2, "reviewer"); err == nil {
		t.Fatal("unknown sole label must refuse")
	}
	task3 := &Task{Labels: []string{"alpha", "beta"}}
	if _, err := TaskOwnershipRole(task3, "reviewer"); err == nil {
		t.Fatal("unknown multi-label must refuse")
	}
}

func TestHeartbeat_DoesNotEraseHandbackIntent(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	mp := NewMemoryProvider()
	stack, err := OpenClaimStack(filepath.Join(dir, "claim"), mp)
	if err != nil {
		t.Fatal(err)
	}
	defer stack.Close()
	key := claim.LeaseKey{Repo: dir, Provider: "memory", Project: "p", TaskRef: "FAC-HBHB"}
	lease, err := stack.AcquireLease(ctx, key, "w-hb", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	wt := t.TempDir()
	ch, _ := MintChallenge()
	cap := LeaseCapability{
		OwnerID: lease.OwnerID, Generation: lease.Generation,
		TaskRef: key.TaskRef, Repo: key.Repo, Provider: key.Provider, Project: key.Project,
		Receipt: "rcpt-hb", Challenge: ch, ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
	if err := WriteLeaseCapability(wt, cap); err != nil {
		t.Fatal(err)
	}
	intent := HandbackIntent{
		OwnerID: cap.OwnerID, Generation: cap.Generation, TaskRef: cap.TaskRef,
		Repo: cap.Repo, Provider: cap.Provider, Project: cap.Project,
		Receipt: cap.Receipt, Challenge: cap.Challenge, RemoteReleased: true,
	}
	if err := WriteHandbackIntent(wt, intent); err != nil {
		t.Fatal(err)
	}
	// Heartbeat expiry refresh must preserve intent.
	if err := RefreshLeaseCapabilityExpiry(wt, cap.OwnerID, cap.Generation, time.Now().UTC().Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandbackIntent(wt)
	if err != nil || got == nil || !got.RemoteReleased {
		t.Fatalf("intent must survive heartbeat: got=%+v err=%v", got, err)
	}
	// Full handback recovery after heartbeat still completes.
	if err := TryAutoHandback(ctx, stack.Manager, wt); err != nil {
		t.Fatalf("handback after heartbeat: %v", err)
	}
	// Heartbeat after completed session must refuse.
	if err := RefreshLeaseCapabilityExpiry(wt, cap.OwnerID, cap.Generation, time.Now().UTC().Add(3*time.Hour)); err == nil {
		// Capability may already be gone — either refuse or not-exist.
		if _, rerr := ReadLeaseCapability(wt); rerr == nil {
			t.Fatal("heartbeat after handback must refuse while capability present")
		}
	}
}

func TestSecureHerd_ParentSwapRefused(t *testing.T) {
	// openSecureHerd holds a directory fd; writes go to that inode even if
	// the path is later replaced. Prove open of a symlink .herd is refused.
	wt := t.TempDir()
	evil := t.TempDir()
	if err := os.Symlink(evil, filepath.Join(wt, ".herd")); err != nil {
		t.Fatal(err)
	}
	_, err := openSecureHerd(wt)
	if err == nil || (!strings.Contains(err.Error(), "symlink") && !strings.Contains(err.Error(), "not a directory")) {
		t.Fatalf("want symlink refuse, got %v", err)
	}
}

// TestSecureHerd_ConcurrentWorktreeSwap_FailClosedOrStable: racing rename of
// the worktree path to a symlink must not redirect successful writes into evil.
func TestSecureHerd_ConcurrentWorktreeSwap_FailClosedOrStable(t *testing.T) {
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	evil := filepath.Join(parent, "evil")
	if err := os.Mkdir(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(evil, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write once to establish real .herd under wt.
	if err := WriteLeaseCapability(wt, testCapability("o1", 1)); err != nil {
		t.Fatal(err)
	}
	// Concurrent swapper: rename wt → wt.real, symlink wt → evil.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = os.Rename(wt, wt+".real")
			_ = os.Symlink(evil, wt)
			time.Sleep(time.Microsecond)
			_ = os.Remove(wt)
			_ = os.Rename(wt+".real", wt)
		}
	}()
	// Racing writers: success must land only on real capability tree, never evil.
	for i := 0; i < 50; i++ {
		_ = WriteLeaseCapability(wt, testCapability("o1", 1))
	}
	<-done
	// Evil must never contain a capability written by us.
	evilCap := filepath.Join(evil, ".herd", "lease-capability.json")
	if _, err := os.Stat(evilCap); err == nil {
		t.Fatal("capability leaked into evil dir via concurrent worktree symlink swap")
	}
}

func TestHandbackIntent_JSONRoundTrip(t *testing.T) {
	wt := t.TempDir()
	intent := HandbackIntent{
		OwnerID: "o", Generation: 5, TaskRef: "FAC-X",
		Repo: "r", Provider: "kaneo", Project: "p",
		Receipt: "rc", Challenge: "ch", RemoteReleased: true,
	}
	if err := WriteHandbackIntent(wt, intent); err != nil {
		t.Fatal(err)
	}
	got, err := ReadHandbackIntent(wt)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	if !strings.Contains(string(raw), `"remote_released":true`) {
		t.Fatalf("intent missing remote_released: %s", raw)
	}
	if got.Repo != "r" || got.Generation != 5 {
		t.Fatalf("got %+v", got)
	}
}
