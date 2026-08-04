package main

import (
	"testing"
	"time"
)

func TestParseHoldExpirySupportsAbsoluteAndBoundedDuration(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	abs, err := parseHoldExpiry("2026-08-04T13:00:00Z", func() time.Time { return now })
	if err != nil || abs == nil || !abs.Equal(now.Add(time.Hour)) {
		t.Fatalf("absolute expiry=%v err=%v", abs, err)
	}
	dur, err := parseHoldExpiry("2h", func() time.Time { return now })
	if err != nil || dur == nil || !dur.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("duration expiry=%v err=%v", dur, err)
	}
	if _, err := parseHoldExpiry("168h1m", func() time.Time { return now }); err == nil {
		t.Fatal("unbounded duration accepted")
	}
}
