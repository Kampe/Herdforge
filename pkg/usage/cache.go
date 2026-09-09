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
//
// FAC-786 adds account binding: a persisted reading is only reusable for a
// provider when the cached account key and the account whose credentials are on
// this machine RIGHT NOW are both known and equal. Changing accounts must never
// reuse the old account's quota, and an unprovable identity is unknown — never
// silently compatible. The in-process cache stays age-bounded only: it lives
// inside one command, and an account switch mid-command is out of scope.
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
// of the ACCOUNT, not of a checkout, and two repositories share it. Being
// per-host is also what keeps distinct Mac and WSL accounts independent.
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
	FetchedAt time.Time                       `json:"fetched_at,omitempty"` // legacy format
	Snapshot  *UsageSnapshot                  `json:"snapshot,omitempty"`   // legacy format
	Providers map[string]cachedProviderRecord `json:"providers,omitempty"`
}

type cachedProviderRecord struct {
	ObservedAt   time.Time     `json:"observed_at"`
	Provider     ProviderUsage `json:"provider"`
	AccountKey   string        `json:"account_key,omitempty"`
	BackoffUntil time.Time     `json:"backoff_until,omitempty"`
	Error        string        `json:"error,omitempty"`
}

// readSnapshotFile returns a persisted reading and its age when it is younger
// than ttl AND every returned provider provably belongs to the account whose
// credentials are currently installed. Any problem — missing, unreadable,
// corrupt, aged out, clock-skewed, or identity-unprovable — reports not-ok so
// the caller fetches live. A cache is an optimisation and must never be a
// source of truth it cannot prove.
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
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, 0, false
	}
	if len(c.Providers) == 0 && c.Snapshot != nil && !c.FetchedAt.IsZero() {
		c.Providers = make(map[string]cachedProviderRecord, len(c.Snapshot.Providers))
		for name, provider := range c.Snapshot.Providers {
			record := cachedProviderRecord{ObservedAt: c.FetchedAt, Provider: provider}
			if provider.Account != nil {
				record.AccountKey = provider.Account.Key
			}
			c.Providers[name] = record
		}
	}
	bound, age, ok := boundCachedProviders(c.Providers, ttl)
	if !ok {
		return nil, 0, false
	}
	return bound, age, true
}

func boundCachedProviders(records map[string]cachedProviderRecord, ttl time.Duration) (*UsageSnapshot, time.Duration, bool) {
	if len(records) == 0 {
		return nil, 0, false
	}
	out := &UsageSnapshot{GeneratedAt: time.Now().UTC(), Providers: map[string]ProviderUsage{}}
	var oldest time.Duration
	for name, record := range records {
		age := time.Since(record.ObservedAt)
		if record.ObservedAt.IsZero() || age < 0 || age >= ttl || !providerCacheUsable(name, record.Provider) || record.AccountKey != record.Provider.Account.Key {
			continue
		}
		if record.BackoffUntil.After(time.Now()) {
			if record.Error != "" {
				if out.Errors == nil {
					out.Errors = map[string]string{}
				}
				out.Errors[name] = record.Error
			}
			continue
		}
		out.Providers[name] = record.Provider
		if age > oldest {
			oldest = age
		}
	}
	if len(out.Providers) == 0 {
		return nil, 0, false
	}
	return out, oldest, true
}

func freshBoundSnapshot(snap *UsageSnapshot, ttl time.Duration, fallback time.Time) (*UsageSnapshot, time.Duration, bool) {
	if snap == nil || len(snap.Providers) == 0 {
		return nil, 0, false
	}
	out := &UsageSnapshot{GeneratedAt: snap.GeneratedAt, Providers: make(map[string]ProviderUsage)}
	if len(snap.Errors) != 0 {
		out.Errors = snap.Errors
	}
	var oldest time.Duration
	for name, provider := range snap.Providers {
		observed := provider.ObservedAt
		if observed.IsZero() {
			observed = fallback
		}
		age := time.Since(observed)
		if observed.IsZero() || age < 0 || age >= ttl || !providerCacheUsable(name, provider) {
			continue
		}
		out.Providers[name] = provider
		if age > oldest {
			oldest = age
		}
	}
	if len(out.Providers) == 0 {
		return nil, 0, false
	}
	return out, oldest, true
}

func providerCacheUsable(name string, provider ProviderUsage) bool {
	if name == "claude" && (provider.Account == nil || !strings.HasPrefix(strings.TrimSpace(provider.Account.Provenance), "claude-profile:")) {
		return false
	}
	current := providerAccountIdentity(name)
	return provider.Account != nil && current != nil && provider.Account.Key == current.Key
}

// bindSnapshotToAccounts filters a persisted snapshot down to the providers
// whose account identity is provable and unchanged. nil means nothing is
// provable and the caller must fetch live.
//
//   - cached identity unknown, or current identity unknown: the entry cannot be
//     attributed, so it is dropped (the provider reads as unknown, which is
//     honest — it must never read as healthy or exhausted)
//   - both known and equal: reusable
//   - both known and different: the account CHANGED. The whole reading is
//     discarded, not just the entry — quota for a different billing account is
//     not a partial-truth cache hit.
func bindSnapshotToAccounts(snap *UsageSnapshot) (*UsageSnapshot, bool) {
	if snap == nil || len(snap.Providers) == 0 {
		return nil, false
	}
	out := &UsageSnapshot{GeneratedAt: snap.GeneratedAt}
	if len(snap.Errors) > 0 {
		out.Errors = snap.Errors
	}
	kept := make(map[string]ProviderUsage, len(snap.Providers))
	for name, pu := range snap.Providers {
		if name == "claude" && (pu.Account == nil || !strings.HasPrefix(strings.TrimSpace(pu.Account.Provenance), "claude-profile:")) {
			continue
		}
		current := providerAccountIdentity(name)
		if pu.Account == nil || current == nil {
			continue
		}
		if pu.Account.Key != current.Key {
			return nil, false
		}
		kept[name] = pu
	}
	if len(kept) == 0 {
		return nil, false
	}
	out.Providers = kept
	return out, true
}

// writeSnapshotFile persists a fresh reading. Best-effort: failing to cache is
// never a reason to fail the decision that just succeeded. Atomic
// (write-temp-then-rename) with private permissions; the body is quota data
// plus opaque account keys — never credentials.
func writeSnapshotFile(snap *UsageSnapshot) {
	if err := mergeSnapshotFile(snap, nil); err != nil {
		return
	}
}

func withSnapshotFileLock(fn func() error) error {
	path := snapshotCachePath()
	if path == "" {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return withCacheFileLock(path+".lock", fn)
}

func mergeSnapshotFile(snap *UsageSnapshot, backoff *cachedProviderRecord) error {
	path := snapshotCachePath()
	if path == "" || snap == nil {
		return os.ErrInvalid
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	records := map[string]cachedProviderRecord{}
	if raw, err := os.ReadFile(path); err == nil {
		var prior cachedSnapshot
		if json.Unmarshal(raw, &prior) == nil {
			records = prior.Providers
			if len(records) == 0 && prior.Snapshot != nil {
				for name, provider := range prior.Snapshot.Providers {
					record := cachedProviderRecord{ObservedAt: prior.FetchedAt, Provider: provider}
					if provider.Account != nil {
						record.AccountKey = provider.Account.Key
					}
					records[name] = record
				}
			}
		}
	}
	for name, provider := range snap.Providers {
		record := cachedProviderRecord{ObservedAt: provider.ObservedAt, Provider: provider}
		if record.ObservedAt.IsZero() {
			record.ObservedAt = time.Now().UTC()
		}
		if provider.Account != nil {
			record.AccountKey = provider.Account.Key
		}
		records[name] = record
	}
	if backoff != nil {
		for name := range snap.Errors {
			records[name] = *backoff
		}
	}
	body, err := json.Marshal(cachedSnapshot{Providers: records})
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return nil
}

// FetchSnapshotCached returns a recent quota reading, fetching only when the
// held one has aged out. The second return is how old the reading is, so a
// caller can report what it acted on rather than implying it was live.
//
// On error the partial snapshot — with its per-provider reasons — is returned
// alongside the error. Callers that check the error first are unaffected, and
// reporting surfaces can show exact reasons instead of a bare failure.
func FetchSnapshotCached() (*UsageSnapshot, time.Duration, error) {
	return FetchSnapshotCachedForce(false)
}

// FetchSnapshotCachedForce bypasses successful observation caches when force is
// true. Persisted rate-limit backoff is still honored: force cannot turn a
// provider's explicit 429 cooldown into another upstream request.
func FetchSnapshotCachedForce(force bool) (*UsageSnapshot, time.Duration, error) {
	ttl := snapshotTTL()
	quotaCache.Lock()
	defer quotaCache.Unlock()

	if !force && ttl > 0 && quotaCache.snap != nil {
		if bound, age, ok := freshBoundSnapshot(quotaCache.snap, ttl, quotaCache.fetchedAt); ok {
			return bound, age, nil
		}
	}
	// FAC-679 (second pass): the in-process cache alone did nothing for the case
	// that reported the problem. Every `herd review` is its OWN process, so a
	// memory cache collapses repeated fetches within one launch and helps not at
	// all across launches -- which is where the 29-272 seconds were being spent.
	//
	// Caught by measuring two consecutive launches and seeing no improvement
	// that the cache could account for. The reading is therefore persisted, with
	// the same rules: short TTL, age reported, identity-bound, and a failed
	// refresh never served from disk.
	var snap *UsageSnapshot
	var err error
	var ageUsed time.Duration
	lockErr := withSnapshotFileLock(func() error {
		if !force && ttl > 0 {
			if cached, age, ok := readSnapshotFile(ttl); ok {
				quotaCache.snap, quotaCache.fetchedAt = cached, time.Now().Add(-age)
				snap = cached
				ageUsed = age
				return nil
			}
		}
		pollers := activeNativePollers()
		available := make(map[string]func() (ProviderUsage, error), len(pollers))
		suppressed := make(map[string]string)
		for name, poll := range pollers {
			if record, ok := readProviderRecord(name); ok && record.BackoffUntil.After(time.Now()) && record.AccountKey == currentProviderAccountKey(name) {
				suppressed[name] = record.Error
				continue
			}
			available[name] = poll
		}
		snap, err = fetchDirectAllWithPollers(available)
		if len(suppressed) > 0 {
			if snap.Errors == nil {
				snap.Errors = make(map[string]string)
			}
			for name, detail := range suppressed {
				if detail == "" {
					detail = "rate-limited: provider is in persisted backoff"
				}
				snap.Errors[name] = detail
			}
		}
		if len(snap.Providers) > 0 {
			quotaCache.snap = snap
			quotaCache.fetchedAt = time.Now()
			if writeErr := mergeSnapshotFile(snap, nil); writeErr != nil {
				return writeErr
			}
		}
		if writeErr := persistRateLimitErrors(snap); writeErr != nil {
			return writeErr
		}
		return nil
	})
	if lockErr != nil {
		return nil, 0, lockErr
	}
	if err != nil {
		// Deliberately do NOT fall back to the cached value. A provider that has
		// stopped answering is precisely when routing on remembered numbers can
		// spend a request against a surface that has gone to zero.
		return snap, 0, err
	}
	return snap, ageUsed, nil
}

func persistRateLimitErrors(snap *UsageSnapshot) error {
	if snap == nil {
		return nil
	}
	for name, detail := range snap.Errors {
		if strings.HasPrefix(detail, "rate-limited: ") {
			record := cachedProviderRecord{ObservedAt: time.Now().UTC(), AccountKey: currentProviderAccountKey(name), BackoffUntil: time.Now().Add(rateLimitBackoff(pollErrf("rate-limited", "%s", detail))), Error: detail}
			if err := mergeSnapshotFile(&UsageSnapshot{Errors: map[string]string{name: detail}}, &record); err != nil {
				return err
			}
		}
	}
	return nil
}

// fetchProviderCached is the provider-scoped companion to
// FetchSnapshotCachedForce. Explicit review/provider requests must share the
// same fresh native observation as the limits command; otherwise every review
// would immediately re-poll and a provider 429 would turn a recent success
// into an UNKNOWN route. The cache mutex is held through a miss, providing
// singleflight semantics for this process without starting a second poll.
func fetchProviderCached(provider string, force bool) (*UsageSnapshot, error) {
	ttl := snapshotTTL()
	name := strings.ToLower(strings.TrimSpace(provider))
	switch name {
	case "agy":
		name = "antigravity"
	case "lazer":
		name = "litellm"
	}
	quotaCache.Lock()
	defer quotaCache.Unlock()
	if !force && ttl > 0 && quotaCache.snap != nil {
		if bound, _, ok := freshBoundSnapshot(quotaCache.snap, ttl, quotaCache.fetchedAt); ok {
			if snap := providerOnlySnapshot(bound, name); snap != nil {
				return snap, nil
			}
		}
	}
	var snap *UsageSnapshot
	var err error
	lockErr := withSnapshotFileLock(func() error {
		if record, ok := readProviderRecord(name); ok && record.BackoffUntil.After(time.Now()) && record.AccountKey == currentProviderAccountKey(name) {
			return nil
		}
		if !force && ttl > 0 {
			if cached, age, ok := readSnapshotFile(ttl); ok {
				if selected := providerOnlySnapshot(cached, name); selected != nil {
					quotaCache.snap = cached
					quotaCache.fetchedAt = time.Now().Add(-age)
					snap = selected
					return nil
				}
			}
		}
		snap, err = fetchDirectProvider(provider)
		if err == nil {
			if quotaCache.snap == nil {
				quotaCache.snap = &UsageSnapshot{Providers: map[string]ProviderUsage{}}
			}
			for k, v := range snap.Providers {
				quotaCache.snap.Providers[k] = v
			}
			quotaCache.fetchedAt = time.Now()
			return mergeSnapshotFile(snap, nil)
		}
		if pollErrorCode(err) == "rate-limited" {
			record := cachedProviderRecord{ObservedAt: time.Now().UTC(), AccountKey: currentProviderAccountKey(name), BackoffUntil: time.Now().Add(rateLimitBackoff(err)), Error: classifyPollError(err)}
			return mergeSnapshotFile(&UsageSnapshot{Providers: map[string]ProviderUsage{}, Errors: map[string]string{name: classifyPollError(err)}}, &record)
		}
		return nil
	})
	if lockErr != nil {
		return nil, lockErr
	}
	if snap == nil && err == nil {
		if record, ok := readProviderRecord(name); ok && record.Error != "" {
			err = pollErrf("rate-limited", "%s", record.Error)
		} else {
			err = pollErrf("rate-limited", "provider %s is in persisted backoff", name)
		}
	}
	if err != nil {
		return snap, err
	}
	return snap, nil
}

func currentProviderAccountKey(name string) string {
	if account := providerAccountIdentity(name); account != nil {
		return account.Key
	}
	return ""
}

func readProviderRecord(name string) (cachedProviderRecord, bool) {
	raw, err := os.ReadFile(snapshotCachePath())
	if err != nil {
		return cachedProviderRecord{}, false
	}
	var c cachedSnapshot
	if json.Unmarshal(raw, &c) != nil {
		return cachedProviderRecord{}, false
	}
	if record, ok := c.Providers[name]; ok {
		return record, true
	}
	return cachedProviderRecord{}, false
}

func rateLimitBackoff(err error) time.Duration {
	const defaultBackoff = 15 * time.Second
	const maxBackoff = 5 * time.Minute
	text := classifyPollError(err)
	marker := "retry-after="
	if i := strings.Index(text, marker); i >= 0 {
		value := strings.TrimSpace(strings.TrimPrefix(text[i+len(marker):], ""))
		if seconds, parseErr := strconv.Atoi(value); parseErr == nil && seconds > 0 {
			if d := time.Duration(seconds) * time.Second; d <= maxBackoff {
				return d
			}
			return maxBackoff
		}
	}
	return defaultBackoff
}

func providerOnlySnapshot(snap *UsageSnapshot, provider string) *UsageSnapshot {
	if snap == nil {
		return nil
	}
	p, ok := snap.Providers[provider]
	if !ok {
		return nil
	}
	return &UsageSnapshot{
		GeneratedAt: snap.GeneratedAt,
		Providers:   map[string]ProviderUsage{provider: p},
		Errors:      snap.Errors,
	}
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
