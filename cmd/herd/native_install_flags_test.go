package main

import (
	"strings"
	"testing"
)

func TestParseNativeInstallRejectsContradictoryActDryRun(t *testing.T) {
	base := []string{"--source", "src", "--revision", strings.Repeat("a", 40), "--target", "root"}
	for _, args := range [][]string{
		append(append([]string{}, base...), "--act", "--dry-run"),
		append(append([]string{}, base...), "--dry-run", "--act"),
	} {
		opts, err := parseNativeInstallArgs(args)
		if err == nil || !strings.Contains(err.Error(), "contradictory") {
			t.Fatalf("contradictory act/dry-run must be rejected before mutation, args=%v opts=%+v err=%v", args, opts, err)
		}
	}
	if opts, err := parseNativeInstallArgs(append(append([]string{}, base...), "--dry-run")); err != nil || opts.act {
		t.Fatalf("lone --dry-run must still parse as dry-run: %+v %v", opts, err)
	}
}
