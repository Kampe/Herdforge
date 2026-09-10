package usage

import "time"

// LimitsSchema is the versioned identifier of the portable limits surface
// emitted by `herd quota --limits`. It names the NATIVE collector contract —
// deliberately not any third-party helper's schema — so dotfiles consumers can
// pin and detect drift.
const LimitsSchema = "herd.quota.limits.v1"

// LimitsReport is the stable machine-readable shape of a quota fetch: what was
// read, from which authority, for which account, how old it is, and exactly why
// any attempted provider produced nothing. Providers present have real
// readings; providers in Errors failed with a machine-readable reason; a
// provider in neither map is unknown.
type LimitsReport struct {
	Schema          string                   `json:"schema"`
	GeneratedAt     time.Time                `json:"generatedAt"`
	CacheAgeSeconds float64                  `json:"cacheAgeSeconds"`
	Providers       map[string]ProviderUsage `json:"providers"`
	Errors          map[string]string        `json:"errors,omitempty"`
}

// LimitsReport renders a snapshot (and the age of the cache reading it was
// served from, 0 when fetched live) as the portable limits document.
func (s *UsageSnapshot) LimitsReport(cacheAge time.Duration) LimitsReport {
	r := LimitsReport{
		Schema:          LimitsSchema,
		CacheAgeSeconds: cacheAge.Seconds(),
	}
	if s != nil {
		r.GeneratedAt = s.GeneratedAt
		r.Providers = s.Providers
		r.Errors = s.Errors
	}
	if r.Providers == nil {
		r.Providers = map[string]ProviderUsage{}
	}
	return r
}
