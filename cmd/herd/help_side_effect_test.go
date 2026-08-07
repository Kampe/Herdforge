package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-189: every subcommand's -h/--help must exit 0 with zero operational
// side-effect probe entries. A production regression that treats --help as a
// positional dispatch ticket marks the probe and fails these tests.

func TestArgsWantHelp(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"empty", nil, false},
		{"long", []string{"--help"}, true},
		{"short", []string{"-h"}, true},
		{"after flags", []string{"--lane", "worker", "--help"}, true},
		{"before ticket", []string{"--help", "FAC-1"}, true},
		{"literal after delimiter", []string{"--", "--help"}, false},
		{"ticket equals form not help token", []string{"--ticket=--help"}, false},
		{"normal ticket", []string{"FAC-189"}, false},
		{"nested with help", []string{"check", "--help"}, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := argsWantHelp(tc.args); got != tc.want {
				t.Fatalf("argsWantHelp(%v)=%v want %v", tc.args, got, tc.want)
			}
		})
	}
}

func TestParseDispatchArgs(t *testing.T) {
	t.Parallel()
	t.Run("ticket and flags", func(t *testing.T) {
		t.Parallel()
		req, err := parseDispatchArgs([]string{"FAC-189", "--no-launch", "--lane", "smith"})
		if err != nil {
			t.Fatal(err)
		}
		if req.TicketRef != "FAC-189" || !req.NoLaunch || req.LaneName != "smith" || !req.LaneExplicit {
			t.Fatalf("unexpected request: %+v", req)
		}
	})
	t.Run("flags before ticket", func(t *testing.T) {
		t.Parallel()
		req, err := parseDispatchArgs([]string{"--lane", "worker", "--no-launch", "FAC-1"})
		if err != nil {
			t.Fatal(err)
		}
		if req.TicketRef != "FAC-1" || !req.NoLaunch {
			t.Fatalf("unexpected request: %+v", req)
		}
	})
	t.Run("explicit ticket equals help", func(t *testing.T) {
		t.Parallel()
		req, err := parseDispatchArgs([]string{"--ticket=--help"})
		if err != nil {
			t.Fatal(err)
		}
		if req.TicketRef != "--help" {
			t.Fatalf("ticket=%q", req.TicketRef)
		}
	})
	t.Run("delimiter literal help", func(t *testing.T) {
		t.Parallel()
		// flag.Parse stops at "--" and leaves remaining args including the
		// literal payload for parseTicketRef via fs.Args() after re-parse.
		// Go's flag package: Parse([]string{"--", "--help"}) yields Args ["--help"].
		req, err := parseDispatchArgs([]string{"--", "--help"})
		if err != nil {
			t.Fatal(err)
		}
		if req.TicketRef != "--help" {
			t.Fatalf("ticket=%q want --help", req.TicketRef)
		}
	})
	t.Run("bare help is flag help not ticket", func(t *testing.T) {
		t.Parallel()
		_, err := parseDispatchArgs([]string{"--help"})
		if err == nil {
			t.Fatal("expected error for bare --help (FlagSet help / reserved)")
		}
	})
	t.Run("dash ref without delimiter is unknown flag", func(t *testing.T) {
		t.Parallel()
		_, err := parseDispatchArgs([]string{"-weird"})
		if err == nil {
			t.Fatal("expected error for dash-prefixed ticket without -- or --ticket")
		}
	})
	t.Run("missing ticket", func(t *testing.T) {
		t.Parallel()
		_, err := parseDispatchArgs([]string{"--no-launch"})
		if err == nil {
			t.Fatal("expected missing ticket error")
		}
	})
}

func TestParseTicketRefExplicitAndDelimiter(t *testing.T) {
	t.Parallel()
	ref, err := parseTicketRef("--help", nil)
	if err != nil || ref != "--help" {
		t.Fatalf("explicit ticket flag must accept --help: ref=%q err=%v", ref, err)
	}
	ref, err = parseTicketRef("", []string{"--help"})
	if err != nil || ref != "--help" {
		t.Fatalf("positional after delimiter may be --help: ref=%q err=%v", ref, err)
	}
}

func TestKnownSubcommandsCoverRoutedCommands(t *testing.T) {
	// Keep the catalog honest: every name in printUsage surface that is a
	// real subcommand must appear in subcommandUsage.
	required := []string{
		"init", "clone", "preflight", "selftest", "status", "pulse", "wind-down",
		"hold", "review", "approve", "drain", "board-done", "board-sync", "sh",
		"send", "cleanup", "forge", "standing", "daemon", "usage", "quota", "up",
		"activate", "validate-config", "doctor-models", "next", "dispatch", "deps",
		"harvest", "unmerged", "lost", "throughput", "worktrees", "overlap",
		"attention", "process", "resolve-lane", "route", "kick", "lifecycle",
		"resources", "lock", "reset-safe", "verify", "tool-probe", "shoot",
		"review-ledger", "repl", "fresh-build", "tests-for",
	}
	for _, name := range required {
		if _, ok := subcommandUsage[name]; !ok {
			t.Errorf("subcommandUsage missing %q", name)
		}
	}
	if len(knownSubcommands()) < len(required) {
		t.Fatalf("knownSubcommands()=%d want >= %d", len(knownSubcommands()), len(required))
	}
}

func TestSubcommandHelpSideEffectFree(t *testing.T) {
	binary := buildHerd(t)
	// One binary for the whole table keeps the suite under CI time budgets.
	commands := knownSubcommands()
	if len(commands) == 0 {
		t.Fatal("no known subcommands")
	}
	for _, cmd := range commands {
		cmd := cmd
		for _, helpFlag := range []string{"--help", "-h"} {
			helpFlag := helpFlag
			t.Run(cmd+"/"+helpFlag, func(t *testing.T) {
				probe := filepath.Join(t.TempDir(), "probe")
				c := exec.Command(binary, cmd, helpFlag)
				c.Env = append(os.Environ(), helpProbeEnv+"="+probe)
				out, err := c.CombinedOutput()
				if err != nil {
					t.Fatalf("expected exit 0, got %v\n%s", err, out)
				}
				if len(strings.TrimSpace(string(out))) == 0 {
					t.Fatalf("expected usage on stdout, got empty")
				}
				// Usage should mention the command name or generic Usage line.
				body := string(out)
				if !strings.Contains(body, "Usage:") && !strings.Contains(body, "usage:") && !strings.Contains(strings.ToLower(body), cmd) {
					t.Fatalf("help output does not look like usage:\n%s", body)
				}
				if data, readErr := os.ReadFile(probe); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
					t.Fatalf("help path entered operational code (probe=%q)", strings.TrimSpace(string(data)))
				}
			})
		}
	}
}

func TestDispatchHelpNeverTreatsHelpAsTicket(t *testing.T) {
	binary := buildHerd(t)
	probe := filepath.Join(t.TempDir(), "probe")

	// The live incident: `herd dispatch --help` must not enter dispatch
	// operational code (config load, claim, compensator, outbox, Herdr, git).
	for _, args := range [][]string{
		{"dispatch", "--help"},
		{"dispatch", "-h"},
		{"dispatch", "--lane", "worker", "--help"},
		{"dispatch", "--no-launch", "-h"},
	} {
		args := args
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			_ = os.Remove(probe)
			c := exec.Command(binary, args...)
			c.Env = append(os.Environ(), helpProbeEnv+"="+probe)
			out, err := c.CombinedOutput()
			if err != nil {
				t.Fatalf("exit: %v\n%s", err, out)
			}
			if strings.Contains(string(out), "Dispatching") {
				t.Fatalf("dispatch operational banner observed on help path:\n%s", out)
			}
			if !strings.Contains(string(out), "dispatch") && !strings.Contains(string(out), "Usage:") {
				t.Fatalf("expected dispatch usage, got:\n%s", out)
			}
			if data, readErr := os.ReadFile(probe); readErr == nil && strings.Contains(string(data), "dispatch") {
				t.Fatalf("side-effect probe recorded dispatch entry on help: %q", string(data))
			}
		})
	}
}

// TestDispatchHelpRegressionMutation documents the exact production bug:
// taking os.Args[2] as ticketRef before a flag parser causes --help to enter
// operational code. The probe + parseDispatchArgs contract make that fail.
func TestDispatchHelpRegressionMutation(t *testing.T) {
	t.Parallel()
	// Mutated parser: first arg is the ticket even when it is --help.
	mutated := func(args []string) (dispatchRequest, error) {
		if len(args) == 0 {
			return dispatchRequest{}, fmtError("missing")
		}
		return dispatchRequest{TicketRef: args[0]}, nil
	}
	req, err := mutated([]string{"--help"})
	if err != nil {
		t.Fatal(err)
	}
	if req.TicketRef != "--help" {
		t.Fatalf("mutation fixture broken: %+v", req)
	}
	// Correct parser must not accept bare --help as a silent ticket (FlagSet
	// help / error). The mutated path above is what the probe tests catch at
	// the binary boundary when operational code is entered.
	if _, err := parseDispatchArgs([]string{"--help"}); err == nil {
		t.Fatal("parseDispatchArgs must refuse bare --help so the mutated path stays red")
	}
	if !argsWantHelp([]string{"--help"}) {
		t.Fatal("argsWantHelp must treat --help as help before any executor runs")
	}
	if argsWantHelp([]string{"--", "--help"}) {
		t.Fatal("delimiter form must remain a literal payload path")
	}
}

func fmtError(s string) error { return &simpleErr{s} }

type simpleErr struct{ s string }

func (e *simpleErr) Error() string { return e.s }

func TestRootHelpAndVersionUnaffected(t *testing.T) {
	binary := buildHerd(t)
	probe := filepath.Join(t.TempDir(), "probe")
	for _, args := range [][]string{{"--help"}, {"-h"}, {"--version"}, {"-v"}} {
		_ = os.Remove(probe)
		c := exec.Command(binary, args...)
		c.Env = append(os.Environ(), helpProbeEnv+"="+probe)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		if data, readErr := os.ReadFile(probe); readErr == nil && len(strings.TrimSpace(string(data))) > 0 {
			t.Fatalf("%v wrote probe: %q", args, data)
		}
	}
}

func TestDispatchDelimiterAllowsLiteralHelpTicketParse(t *testing.T) {
	t.Parallel()
	// Global gate lets `-- --help` through; parser must accept the literal.
	if argsWantHelp([]string{"--", "--help"}) {
		t.Fatal("delimiter path must not be classified as help")
	}
	// Flags must appear before `--`; everything after is a literal payload.
	req, err := parseDispatchArgs([]string{"--no-launch", "--", "--help"})
	if err != nil {
		t.Fatal(err)
	}
	if req.TicketRef != "--help" || !req.NoLaunch {
		t.Fatalf("unexpected: %+v", req)
	}
}
