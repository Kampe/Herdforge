package main

import (
	"fmt"
	"time"

	"github.com/Kampe/Herdforge/pkg/quotasup"
)

// quotaSnapshotFresh refuses a quota snapshot that is too old to plan from.
//
// FAC-593: capacity planning read .herd/quota-supervisor.json with no age check
// while every other consumer honours DefaultMaxObservationAge. A four-day-old
// snapshot kept asserting that grok was exhausted and claude blocked long after
// both had changed, so lanes piled onto one provider while two underspent pools
// sat idle.
//
// An unparseable or missing observed_at is treated as stale, not as fresh. A
// snapshot that cannot state when it was taken cannot prove it is current, and
// guessing "now" is exactly how a stale reading keeps being trusted.
func quotaSnapshotFresh(s quotasup.Snapshot, maxAge time.Duration) error {
	if maxAge <= 0 {
		maxAge = quotasup.DefaultMaxObservationAge
	}
	stamp := s.ObservedAt
	if stamp == "" {
		stamp = s.SourceAt
	}
	if stamp == "" {
		return fmt.Errorf("live quota snapshot has no observed_at; refusing to plan capacity from an undateable reading " +
			"(refresh with: herd quota-supervisor --json --read-only)")
	}
	observed, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return fmt.Errorf("live quota snapshot observed_at %q is unparseable: %w "+
			"(refresh with: herd quota-supervisor --json --read-only)", stamp, err)
	}
	if age := time.Since(observed); age > maxAge {
		return fmt.Errorf("live quota snapshot is %s old (max %s); refusing to plan capacity from a stale reading "+
			"(refresh with: herd quota-supervisor --json --read-only)",
			age.Round(time.Second), maxAge)
	}
	return nil
}
