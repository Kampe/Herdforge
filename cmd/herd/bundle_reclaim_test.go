package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseBundleReclaimArgsContradictoryModeRefusal(t *testing.T) {
	if _, err := parseBundleReclaimArgs([]string{"--root", "x", "--act", "--dry-run"}); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("contradictory flags must be refused: %v", err)
	}
	if _, err := parseBundleReclaimArgs([]string{"--root", "x", "--dry-run", "--act"}); err == nil || !strings.Contains(err.Error(), "contradictory") {
		t.Fatalf("contradictory flags in either order must be refused: %v", err)
	}
}

func TestParseBundleReclaimArgsRequiresRoot(t *testing.T) {
	if _, err := parseBundleReclaimArgs(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("missing --root must print usage: %v", err)
	}
}

func TestParseBundleReclaimArgsParsesFlags(t *testing.T) {
	opts, err := parseBundleReclaimArgs([]string{
		"--root", "/tmp/x", "--act", "--json",
		"--max-files", "12", "--max-bytes", "2048",
		"--min-age", "90m", "--protect", "a.bundle, b.bundle",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.Act || !opts.jsonOut || opts.Root != "/tmp/x" || opts.MaxFiles != 12 || opts.MaxBytes != 2048 {
		t.Fatalf("flags parsed wrong: %+v", opts)
	}
	if opts.MinAge != 90*time.Minute || len(opts.Protect) != 2 || opts.Protect[1] != "b.bundle" {
		t.Fatalf("min-age/protect parsed wrong: %+v", opts)
	}
	if _, err := parseBundleReclaimArgs([]string{"--root", "x", "--min-age", "soon"}); err == nil {
		t.Fatal("invalid duration must be refused")
	}
	if _, err := parseBundleReclaimArgs([]string{"--root", "x", "--max-files", "0"}); err == nil {
		t.Fatal("non-positive budget must be refused")
	}
	if _, err := parseBundleReclaimArgs([]string{"--root", "x", "--bogus"}); err == nil {
		t.Fatal("unknown flag must be refused")
	}
}
