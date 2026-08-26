package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FAC-679: a live quota fetch reaches every provider serially, and it ran before
// EVERY review launch. Measured on this fleet: 29 seconds on one launch and 272
// on another, tracking provider API latency, while the launch itself was
// otherwise 1.4 seconds of work. An operator bounding the command at a
// reasonable timeout kills it mid-flight and loses the lease.
//
// The reason I did not simply cache it earlier is real and unchanged: a STALE
// quota read routes work to an exhausted surface, which is worse than being
// slow. So this is not a plain cache. It is a short, explicit one:
//
//   - the TTL is deliberately small, so a reading can only be seconds old
//   - staleness is reported, never hidden, so a caller can say what it used
//   - a failed refresh does NOT serve the cached value; the error propagates,
//     because "the provider stopped answering" is exactly when routing on
//     remembered numbers is most dangerous
//
// That last point is what separates this from the freshness adapter's STALE
// posture, which is right for reporting and wrong for routing: a report can say
// "this is 4 minutes old", but a launch decision acting on 4-minute-old quota can
// spend a request against a surface that has since gone to zero.
var quotaCache struct {
	sync.Mutex
	snap      *UsageSnapshot
	fetchedAt time.Time
}

// snapshotTTL is how long a quota reading may be reused. Short on purpose: it
// exists to collapse the repeated fetches of one dispatch beat, not to remember
// quota across a session.
func snapshotTTL() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("HERD_QUOTA_CACHE_SECONDS")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 45 * time.Second
}

// snapshotCachePath is per-user and outside any repository: quota is a property
// of the ACCOUNT, not of a checkout, and two repositories share it.
func snapshotCachePath() string {
	if p := strings.TrimSpace(os.Getenv("HERD_QUOTA_CACHE_PATH")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".herd", "state", "quota-snapshot.json")
}

type cachedSnapshot struct {
	FetchedAt time.Time      `json:"fetched_at"`
	Snapshot  *UsageSnapshot `json:"snapshot"`
}

// readSnapshotFile returns a persisted reading and its age when it is younger
// than ttl. Any problem -- missing, unreadable, corrupt, or aged out -- reports
// not-ok so the caller fetches live. A cache is an optimisation and must never
// be a source of truth it cannot prove.
func readSnapshotFile(ttl time.Duration) (*UsageSnapshot, time.Duration, bool) {
	path := snapshotCachePath()
	if path == "" {
		return nil, 0, false
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, 0, false
	}
	var c cachedSnapshot
	if err := json.Unmarshal(raw, &c); err != nil || c.Snapshot == nil || c.FetchedAt.IsZero() {
		return nil, 0, false
	}
	age := time.Since(c.FetchedAt)
	if age < 0 || age >= ttl {
		// A negative age means the clock moved; treat it as unusable rather than
		// as infinitely fresh.
		return nil, 0, false
	}
	return c.Snapshot, age, true
}

// writeSnapshotFile persists a fresh reading. Best-effort: failing to cache is
// never a reason to fail the decision that just succeeded.
func writeSnapshotFile(snap *UsageSnapshot) {
	path := snapshotCachePath()
	if path == "" || snap == nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	body, err := json.Marshal(cachedSnapshot{FetchedAt: time.Now(), Snapshot: snap})
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// FetchSnapshotCached returns a recent quota reading, fetching only when the
// held one has aged out. The second return is how old the reading is, so a
// caller can report what it acted on rather than implying it was live.
func FetchSnapshotCached() (*UsageSnapshot, time.Duration, error) {
	ttl := snapshotTTL()
	quotaCache.Lock()
	defer quotaCache.Unlock()

	if ttl > 0 && quotaCache.snap != nil {
		if age := time.Since(quotaCache.fetchedAt); age < ttl {
			return quotaCache.snap, age, nil
		}
	}
	// FAC-679 (second pass): the in-process cache alone did nothing for the case
	// that reported the problem. Every `herd review` is its OWN process, so a
	// memory cache collapses repeated fetches within one launch and helps not at
	// all across launches -- which is where the 29-272 seconds were being spent.
	//
	// Caught by measuring two consecutive launches and seeing no improvement
	// that the cache could account for. The reading is therefore persisted, with
	// the same rules: short TTL, age reported, and a failed refresh never served
	// from disk.
	if ttl > 0 {
		if snap, age, ok := readSnapshotFile(ttl); ok {
			quotaCache.snap, quotaCache.fetchedAt = snap, time.Now().Add(-age)
			return snap, age, nil
		}
	}
	snap, err := FetchSnapshot()
	if err != nil {
		// Deliberately do NOT fall back to the cached value. A provider that has
		// stopped answering is precisely when routing on remembered numbers can
		// spend a request against a surface that has gone to zero.
		return nil, 0, err
	}
	quotaCache.snap = snap
	quotaCache.fetchedAt = time.Now()
	writeSnapshotFile(snap)
	return snap, 0, nil
}

// InvalidateSnapshotCache drops the held reading. Used after anything that
// changes quota materially, so the next decision refetches rather than acting on
// a number it just invalidated.
func InvalidateSnapshotCache() {
	quotaCache.Lock()
	defer quotaCache.Unlock()
	quotaCache.snap = nil
	quotaCache.fetchedAt = time.Time{}
	if p := snapshotCachePath(); p != "" {
		_ = os.Remove(p)
	}
}
