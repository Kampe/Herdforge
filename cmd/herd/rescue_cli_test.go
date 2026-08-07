package main

import (
	"errors"
	"flag"
	"testing"
)

// TestParseRescueArgsPositionalDoesNotSwallowFlags is the FAC-76 round-2
// regression: Go's flag package stops parsing at the first non-flag token, so
// `herd rescue <pane-id> --apply` used to leave apply=false, take the dry-run
// branch, print WOULD, and exit 0 while the operator believed a repair ran.
// --apply is the sole mutation gate, so losing it is a silent no-op success.
func TestParseRescueArgsPositionalDoesNotSwallowFlags(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"flags after positional", []string{"wK:p2", "--apply", "--label", "forge"}},
		{"flags before positional", []string{"--apply", "--label", "forge", "wK:p2"}},
		{"positional between flags", []string{"--apply", "wK:p2", "--label", "forge"}},
		{"single dash form", []string{"wK:p2", "-apply", "-label", "forge"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, err := parseRescueArgs(tc.args)
			if err != nil {
				t.Fatalf("parseRescueArgs(%q): %v", tc.args, err)
			}
			if !a.apply {
				t.Errorf("apply=false for %q: --apply was parsed as a positional; "+
					"rescue would dry-run and exit 0 without repairing", tc.args)
			}
			if a.paneID != "wK:p2" {
				t.Errorf("paneID=%q want wK:p2 for %q", a.paneID, tc.args)
			}
			if a.label != "forge" {
				t.Errorf("label=%q want forge for %q", a.label, tc.args)
			}
		})
	}
}

// A flag value must never be mistaken for the pane-id positional.
func TestParseRescueArgsFlagValueIsNotPositional(t *testing.T) {
	t.Parallel()
	a, err := parseRescueArgs([]string{"--label", "wK:p2", "--workspace", "wK"})
	if err != nil {
		t.Fatalf("parseRescueArgs: %v", err)
	}
	if a.paneID != "" {
		t.Errorf("paneID=%q want empty: --label's value was consumed as a pane id", a.paneID)
	}
	if a.label != "wK:p2" || a.workspace != "wK" {
		t.Errorf("label=%q workspace=%q want wK:p2 / wK", a.label, a.workspace)
	}
}

// A second positional is a typo, not a target. Dropping it silently would let
// `herd rescue wK:p1 wK:p2 --apply` repair a pane the operator did not name.
func TestParseRescueArgsRejectsExtraPositional(t *testing.T) {
	t.Parallel()
	if _, err := parseRescueArgs([]string{"wK:p1", "wK:p2", "--apply"}); err == nil {
		t.Fatal("expected error for two pane ids, got nil")
	}
}

func TestParseRescueArgsDryRunApplyExclusivity(t *testing.T) {
	t.Parallel()
	if _, err := parseRescueArgs([]string{"--dry-run", "--apply"}); err == nil {
		t.Fatal("--dry-run --apply must be refused")
	}
	// --dry-run=false --apply states one coherent intent: mutate.
	a, err := parseRescueArgs([]string{"--dry-run=false", "--apply"})
	if err != nil {
		t.Fatalf("--dry-run=false --apply must be accepted: %v", err)
	}
	if !a.apply {
		t.Error("apply=false after --dry-run=false --apply")
	}
}

// TestParseRescueArgsHelpIsErrHelp guards the FAC-189 invariant for rescue:
// -h/--help must surface as flag.ErrHelp so no herdr/process inspection runs.
// Defining an `-h` bool on the FlagSet (the round-1 bug) makes -h parse clean,
// discard the value, and fall through into live `pane list` / `pane layout`.
func TestParseRescueArgsHelpIsErrHelp(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help"} {
		if _, err := parseRescueArgs([]string{arg}); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("parseRescueArgs(%q) err=%v want flag.ErrHelp", arg, err)
		}
	}
}

// rescue must stay registered so main.go's global help gate covers it and
// TestSubcommandHelpSideEffectFree exercises it.
func TestRescueRegisteredInSubcommandUsage(t *testing.T) {
	t.Parallel()
	if _, ok := subcommandUsage["rescue"]; !ok {
		t.Fatal("subcommandUsage missing \"rescue\": the -h/--help gate in main.go " +
			"only runs for registered commands, so help would reach runRescue")
	}
}
