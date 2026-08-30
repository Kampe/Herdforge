package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// FAC-679: the cache exists because a live quota fetch ran before EVERY review
// launch and took 29-272 seconds while the launch itself was 1.4s of work. But a
// STALE quota read routes work to an exhausted surface, which is worse than
// being slow -- so the TTL is short and staleness is reported, never hidden.
func TestSnapshotTTLIsShortAndOverridable(t *testing.T) {
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "")
	if got := snapshotTTL(); got != 45*time.Second {
		t.Errorf("default TTL = %v, want 45s: long enough to collapse one dispatch beat, short enough that quota cannot go stale unnoticed", got)
	}
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "5")
	if got := snapshotTTL(); got != 5*time.Second {
		t.Errorf("override = %v, want 5s", got)
	}
	// Zero disables reuse entirely, so an operator who wants every decision live
	// can have that without editing code.
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	if got := snapshotTTL(); got != 0 {
		t.Errorf("zero must disable reuse, got %v", got)
	}
	// A malformed value falls back to the safe default rather than to zero-or-
	// forever, either of which would be a surprise.
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "not-a-number")
	if got := snapshotTTL(); got != 45*time.Second {
		t.Errorf("malformed override must fall back to the default, got %v", got)
	}
}

// A held reading is reused only while it is young, and its AGE is returned so a
// caller can report what it acted on instead of implying the number was live.
func TestCachedSnapshotReportsItsAge(t *testing.T) {
	InvalidateSnapshotCache()
	quotaCache.Lock()
	quotaCache.snap = &UsageSnapshot{}
	quotaCache.fetchedAt = time.Now().Add(-2 * time.Second)
	quotaCache.Unlock()

	snap, age, err := FetchSnapshotCached()
	if err != nil || snap == nil {
		t.Fatalf("a young reading must be reused: %v", err)
	}
	if age < time.Second {
		t.Errorf("the age must be reported so a caller can say what it used, got %v", age)
	}
}

// Invalidation forces the next decision to refetch, so anything that materially
// changes quota is not followed by a decision acting on the number it just
// invalidated.
func TestInvalidateForcesARefetch(t *testing.T) {
	quotaCache.Lock()
	quotaCache.snap = &UsageSnapshot{}
	quotaCache.fetchedAt = time.Now()
	quotaCache.Unlock()

	InvalidateSnapshotCache()

	quotaCache.Lock()
	held := quotaCache.snap
	quotaCache.Unlock()
	if held != nil {
		t.Fatal("invalidation must drop the held reading")
	}
}

// FAC-679 (second pass): the in-process cache alone did nothing for the case that
// reported the problem. Every `herd review` is its OWN process, so a memory cache
// collapses fetches within one launch and helps not at all across launches --
// which is exactly where the 29-272 seconds were spent. Caught by measuring two
// consecutive launches and seeing no improvement the cache could account for.
func TestPersistedReadingSurvivesAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "q.json"))

	writeSnapshotFile(&UsageSnapshot{})
	snap, age, ok := readSnapshotFile(45 * time.Second)
	if !ok || snap == nil {
		t.Fatal("a freshly written reading must be readable by another process")
	}
	if age > time.Second {
		t.Errorf("a just-written reading should be new, got age %v", age)
	}
}

func TestFreshReceiveTimeCannotRestampAStaleHandoff(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "q.json"))
	t.Setenv("HERD_QUOTA_HANDOFF_REQUIRED", "1")
	previousNow := quotaNow
	quotaNow = func() time.Time { return time.Date(2026, 8, 30, 15, 8, 0, 0, time.UTC) }
	t.Cleanup(func() { quotaNow = previousNow })

	writeSnapshotFile(&UsageSnapshot{
		GeneratedAt: time.Date(2026, 8, 30, 14, 0, 0, 0, time.UTC),
		Schema:      "openusage.limits.v1",
		QuotaSource: QuotaSourceOpenUsageHandoff,
		Providers: map[string]ProviderUsage{
			"grok": {DisplayName: "Grok", Resources: map[string]ResourceUsage{}},
		},
	})
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Fatal("fresh cache receive time revived a stale source generatedAt")
	}
}

// Aged-out, corrupt and missing readings all fetch live. A cache is an
// optimisation and must never be a source of truth it cannot prove.
func TestUnusableReadingsFallThroughToLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "q.json")
	t.Setenv("HERD_QUOTA_CACHE_PATH", path)

	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a missing reading must not be usable")
	}
	os.WriteFile(path, []byte("{not json"), 0o600)
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a corrupt reading must not be usable")
	}
	// Aged out.
	body, _ := json.Marshal(cachedSnapshot{FetchedAt: time.Now().Add(-time.Hour), Snapshot: &UsageSnapshot{}})
	os.WriteFile(path, body, 0o600)
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a reading older than the TTL must not be usable")
	}
	// A clock that moved backwards yields a negative age; that is unusable, not
	// infinitely fresh.
	body, _ = json.Marshal(cachedSnapshot{FetchedAt: time.Now().Add(time.Hour), Snapshot: &UsageSnapshot{}})
	os.WriteFile(path, body, 0o600)
	if _, _, ok := readSnapshotFile(time.Minute); ok {
		t.Error("a future timestamp must be treated as unusable, never as fresh")
	}
}
