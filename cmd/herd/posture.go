package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/posture"
)

// runPosture ports bin/herd-claude-only and bin/herd-no-claude. Both take the
// same status|on|off shape and are deliberately SEPARATE switches: "prefer
// Claude for everything" and "Claude is spent, route around it" are different
// statements, and folding them into one flag made `off` ambiguous.
func runPosture(name posture.Name) {
	fs := flag.NewFlagSet(string(name), flag.ExitOnError)
	fs.Parse(os.Args[2:])
	action := "status"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}

	switch action {
	case "on":
		if err := posture.Set(name, true); err != nil {
			fmt.Fprintf(os.Stderr, "herd %s: %v\n", name, err)
			os.Exit(1)
		}
		// Refuse to leave the fleet in a contradictory posture.
		if err := posture.Resolve(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("herd %s: ON (sentinel %s)\n", name, name.SentinelPath())
		if name == posture.ClaudeOnly {
			fmt.Println("  every lane and reviewer routes to native Claude; non-Claude pools are last resort")
			fmt.Println("  an exhausted Claude still degrades to a real surface, so the fleet cannot stall on this")
		} else {
			fmt.Println("  native Claude is filtered out of every candidate set (a filter, not a demotion)")
			fmt.Println("  lazer remains as the operator-sanctioned last resort")
		}

	case "off":
		if err := posture.Set(name, false); err != nil {
			fmt.Fprintf(os.Stderr, "herd %s: %v\n", name, err)
			os.Exit(1)
		}
		fmt.Printf("herd %s: OFF (sentinel removed)\n", name)
		fmt.Println("  routing returns to task-fit across all healthy pools")

	case "status":
		// Report the EFFECTIVE value the launchers will see, not just the file,
		// so an env override can never be mistaken for the persisted posture.
		fileState := "off"
		if posture.SentinelPresent(name) {
			fileState = "on"
		}
		fmt.Printf("herd %s: sentinel=%s (%s)\n", name, fileState, name.SentinelPath())
		effective := "OFF"
		if posture.Active(name) {
			effective = "ON"
		}
		fmt.Printf("herd %s: EFFECTIVE=%s\n", name, effective)
		if v, set := posture.EnvOverride(name); set {
			fmt.Printf("  NOTE: %s=%v in the environment is overriding the sentinel for this invocation\n", name.EnvVar(), v)
		} else {
			fmt.Printf("  note: an explicit %s in the environment overrides the sentinel for that invocation\n", name.EnvVar())
		}
		if err := posture.Resolve(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "-h", "--help":
		fmt.Printf("Usage: herd %s [status|on|off]\n", name)

	default:
		fmt.Fprintf(os.Stderr, "herd %s: unknown action %q (status|on|off)\n", name, action)
		os.Exit(2)
	}
}

// runBoardFrozen ports bin/herd-board-frozen: exit 0 with the active trigger on
// stdout when frozen, exit 1 with no output when not. Every board-mutating tool
// consults this shim rather than reimplementing the check.
func runBoardFrozen() {
	trigger, frozen := posture.BoardFrozen(".")
	if !frozen {
		os.Exit(1)
	}
	fmt.Println(trigger)
}
