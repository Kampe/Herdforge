package transfer

import (
	"context"
	"encoding/json"
	"fmt"

	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
)

// Regression R1 (review finding 1): act mode must enforce the cumulative
// byte budget with the counter that actually accumulates destructive work.
// On 1c3cd932 the check read WouldReclaimBytes (always zero in act mode),
// so --max-bytes 315 deleted two 315-byte bundles (630 bytes).
func TestReclaimActBudgetBoundsCumulativeBytes(t *testing.T) {
	f := reclaimFixtureSetup(t)
	second := filepath.Join(f.bundleDir, "second.bundle")
	if out, err := exec.Command("git", "-C", f.repoRoot, "bundle", "create", second, "main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	f.writeManifest("eligible-transfer.bundle", "second.bundle")
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.MaxBytes = f.bundleSize // cap admits exactly one bundle
	}))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if report.Reclaimed != 1 || report.ReclaimedBytes != f.bundleSize {
		t.Fatalf("act byte budget must cap at one %d-byte bundle: reclaimed=%d bytes=%d", f.bundleSize, report.Reclaimed, report.ReclaimedBytes)
	}
	if !report.Partial || report.Reason != "byte-budget" {
		t.Fatalf("budget stop must be reported: partial=%t reason=%q", report.Partial, report.Reason)
	}
	if _, err := os.Lstat(second); err != nil {
		t.Fatalf("second bundle must survive the byte budget: %v", err)
	}
}

// Regression R2a (review finding 2): deletion authority must be revalidated
// against the manifest under the native lock immediately before unlink. A
// manifest withdrawn after the pass loaded its snapshot must not be deleted.
func TestReclaimRefusesManifestWithdrawnBeforeUnlink(t *testing.T) {
	f := reclaimFixtureSetup(t)
	withdrawn := false
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.Act = true
		o.LiveReader = func(ctx context.Context, path string) (ReaderStatus, error) {
			if !withdrawn {
				f.writeManifest() // withdraw every entry before the unlink proof
				withdrawn = true
			}
			return ReaderAbsent, nil
		}
	}))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if report.Reclaimed != 0 {
		t.Fatalf("withdrawn manifest must revoke deletion authority: %+v", report)
	}
	if _, err := os.Lstat(f.bundlePath); err != nil {
		t.Fatalf("bundle withdrawn from manifest must survive: %v", err)
	}
}

// Regression R2b (review finding 2): the manifest snapshot must be taken
// AFTER acquiring the native lock, not before blocking on it.
func TestReclaimManifestLoadedUnderLock(t *testing.T) {
	f := reclaimFixtureSetup(t)
	external := lock.NewDirLock(f.lockDir)
	if err := external.Acquire(context.Background(), time.Second, "test pre-holder"); err != nil {
		t.Fatalf("pre-holder acquire: %v", err)
	}
	type result struct {
		report ReclaimReport
		err    error
	}
	done := make(chan result, 1)
	go func() {
		report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
			o.Act = true
			o.LockWait = 10 * time.Second
		}))
		done <- result{report, err}
	}()
	// Give the reclaim pass two full lock-retry cycles so it has certainly
	// passed its manifest load and is blocked inside Acquire, then withdraw
	// and release. LockWait (10s) keeps it blocked through the withdrawal.
	// The withdrawn manifest is a VALID v2 manifest that no longer lists the
	// bundle — the same authority, a narrower grant.
	time.Sleep(2500 * time.Millisecond)
	withdrawn := RetentionManifest{Version: 2, Authority: "forge-orchestrator-test", Bundles: []RetentionEntry{{
		Name: "another.bundle", Digest: strings.Repeat("b", 64), Size: 1, ModTimeUnixNano: 1,
	}}}
	body, err := json.Marshal(withdrawn)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.bundleDir, "retention-manifest.json"), body, 0644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1000 * time.Millisecond)
	external.Release()
	res := <-done
	if res.err != nil {
		t.Fatalf("reclaim: %v", res.err)
	}
	if res.report.Reclaimed != 0 {
		t.Fatalf("stale pre-lock manifest snapshot must not authorize deletion: %+v", res.report)
	}
	if _, statErr := os.Lstat(f.bundlePath); statErr != nil {
		t.Fatalf("bundle must survive manifest withdrawal under lock: %v", statErr)
	}
}

// Regression R3 (review finding 3): a repository nested under an outer
// `.herd` (pool-slot worktrees) must only admit bundle directories beneath
// ITS OWN `.herd`, never `.herd/pool/slot/not-owned/bundles`.
func TestReclaimRefusesPoolWorktreeBundlesOutsideOwnHerd(t *testing.T) {
	f := reclaimFixtureSetup(t)
	slot := filepath.Join(f.repoRoot, ".herd", "pool", "slot")
	if out, err := exec.Command("git", "-C", f.repoRoot, "worktree", "add", "--detach", slot, "main").CombinedOutput(); err != nil {
		t.Fatalf("worktree add: %v\n%s", err, out)
	}
	foreign := filepath.Join(slot, "not-owned", "bundles")
	if err := os.MkdirAll(foreign, 0755); err != nil {
		t.Fatal(err)
	}
	foreignBundle := filepath.Join(foreign, "smuggled.bundle")
	if out, err := exec.Command("git", "-C", slot, "bundle", "create", foreignBundle, "main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	_, err := Reclaim(context.Background(), ReclaimOptions{
		RepoRoot: f.repoRoot, Root: foreign, Manifest: f.manifest,
		LockDir: f.lockDir, LockWait: 2 * time.Second, LiveReader: absentReader(),
	})
	if err == nil || !strings.Contains(err.Error(), ".herd") {
		t.Fatalf("bundle dir outside the containing worktree's own .herd must refuse: %v", err)
	}
	if _, statErr := os.Lstat(foreignBundle); statErr != nil {
		t.Fatalf("smuggled bundle must survive scope refusal: %v", statErr)
	}

	// Positive control: the same worktree's OWN .herd state is admissible
	// with its own manifest binding the bundle's content identity.
	own := filepath.Join(slot, ".herd", "coordinator-resume", "bundles")
	if err := os.MkdirAll(own, 0755); err != nil {
		t.Fatal(err)
	}
	ownBundle := filepath.Join(own, "own.bundle")
	if out, err := exec.Command("git", "-C", slot, "bundle", "create", ownBundle, "main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	ownSt, err := os.Stat(ownBundle)
	if err != nil {
		t.Fatal(err)
	}
	ownDigest, err := fileContentDigest(ownBundle, ownSt.Size())
	if err != nil {
		t.Fatal(err)
	}
	ownManifest := RetentionManifest{Version: 2, Authority: "forge-orchestrator-test", Bundles: []RetentionEntry{{
		Name: "own.bundle", Digest: ownDigest, Size: ownSt.Size(), ModTimeUnixNano: ownSt.ModTime().UnixNano(),
	}}}
	ownManifestBody, err := json.Marshal(ownManifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(own, "retention-manifest.json"), ownManifestBody, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Reclaim(context.Background(), ReclaimOptions{
		RepoRoot: f.repoRoot, Root: own, Manifest: filepath.Join(own, "retention-manifest.json"),
		LockDir: f.lockDir, LockWait: 2 * time.Second, LiveReader: absentReader(),
	}); err != nil {
		t.Fatalf("own .herd bundles must be admissible: %v", err)
	}
}

// Regression R4 (review finding 4): a live holder must never lose the lock
// to the stale-age break during the command's supported runtime window.
func TestReclaimLockAgeNeverBreaksLiveHolderDuringPass(t *testing.T) {
	f := reclaimFixtureSetup(t)
	external := lock.NewDirLock(f.lockDir)
	if err := external.Acquire(context.Background(), time.Second, "test live holder"); err != nil {
		t.Fatalf("holder acquire: %v", err)
	}
	pastOldDefault := time.Now().Add(-6 * time.Minute)
	if err := os.Chtimes(f.lockDir, pastOldDefault, pastOldDefault); err != nil {
		t.Fatal(err)
	}
	_, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) {
		o.LockWait = 2 * time.Second
	}))
	if err == nil || !strings.Contains(err.Error(), "native lock") {
		t.Fatalf("live holder's lock must not be age-broken: %v", err)
	}
	if held, _ := external.Status(); !held {
		t.Fatal("live holder must still hold the lock after the refused pass")
	}
}

// Regression R5 (review finding 4): release must be owner-token-aware — an
// owner whose lock was taken over must not remove the successor's lock.
func TestDirLockReleaseSkipsSuccessorLock(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lock.d")
	a := lock.NewDirLock(dir)
	if err := a.Acquire(context.Background(), time.Second, "first owner"); err != nil {
		t.Fatal(err)
	}
	holder := filepath.Join(dir, lock.HolderFile)
	body, err := os.ReadFile(holder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "token=") {
		t.Fatalf("holder file must carry an owner token: %s", body)
	}
	// Simulate successor takeover: same dir name, fresh owner token. While
	// the stale owner holds the kernel flock, a compliant acquirer cannot
	// exist, so the takeover is exercised the way a raw (non-compliant)
	// writer would do it — directory replacement with a distinct holder
	// token. The stale owner's release must skip it.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	successorBody := fmt.Sprintf("pid=%d\nagent=successor\nreason=successor takeover\ntoken=successor-token\n", os.Getpid())
	if err := os.WriteFile(holder, []byte(successorBody), 0o644); err != nil {
		t.Fatal(err)
	}
	a.Release() // stale owner must not remove the successor's lock
	b := lock.NewDirLock(dir)
	if held, holderStr := b.Status(); !held {
		t.Fatalf("successor lock must survive stale owner release (holder=%s)", holderStr)
	}
	// The raw successor is torn down by its own writer; a compliant
	// acquirer then takes a fresh lock and releases it cleanly.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := b.Acquire(context.Background(), time.Second, "successor"); err != nil {
		t.Fatalf("successor must acquire once the stale owner dropped the flock: %v", err)
	}
	b.Release()
	if held, _ := b.Status(); held {
		t.Fatal("legitimate owner release must remove the lock")
	}
}

// Regression R7 (review finding 5): the per-bundle deletion proof pins exact
// content identity. A valid, canonical-contained bundle whose bytes differ
// from the manifest-pinned digest must never delete — content identity is
// checked at unlink even when size and mtime match the manifest.
func TestReclaimRetainsTamperedContentWithForgedIdentity(t *testing.T) {
	f := reclaimFixtureSetup(t)
	st, err := os.Stat(f.bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	pinnedDigest, err := fileContentDigest(f.bundlePath, st.Size())
	if err != nil {
		t.Fatal(err)
	}
	// Substitute a DIFFERENT valid bundle (two tips, same contained commit)
	// over the eligible path: passes verify and containment, has the same
	// role and canonical refs, but different bytes than the pinned digest.
	substitute := filepath.Join(f.bundleDir, "substitute.tmp.bundle")
	if out, err := exec.Command("git", "-C", f.repoRoot, "bundle", "create", substitute, "main", "refs/remotes/origin/main").CombinedOutput(); err != nil {
		t.Fatalf("bundle create: %v\n%s", err, out)
	}
	body, err := os.ReadFile(substitute)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.bundlePath, body, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(substitute); err != nil {
		t.Fatal(err)
	}
	st2, err := os.Stat(f.bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	manifest := RetentionManifest{Version: 2, Authority: "forge-orchestrator-test", Bundles: []RetentionEntry{{
		Name: "eligible-transfer.bundle", Digest: pinnedDigest, Size: st2.Size(), ModTimeUnixNano: st2.ModTime().UnixNano(),
	}}}
	manifestBody, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(f.bundleDir, "retention-manifest.json")
	if err := os.WriteFile(manifestPath, manifestBody, 0644); err != nil {
		t.Fatal(err)
	}
	report, err := Reclaim(context.Background(), opts(f, func(o *ReclaimOptions) { o.Act = true }))
	if err != nil {
		t.Fatalf("reclaim: %v", err)
	}
	if report.Reclaimed != 0 || !strings.Contains(report.Dispositions[0].Reason, "content-identity-mismatch") {
		t.Fatalf("bytes differing from the pinned digest must never delete: %+v", report)
	}
	if _, statErr := os.Lstat(f.bundlePath); statErr != nil {
		t.Fatalf("substituted bundle must survive: %v", statErr)
	}
}
