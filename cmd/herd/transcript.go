package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/transcript"
)

// runTranscript is the read-only handoff diagnostic (FAC-551). It exists so a
// coordinator does not have to drop to raw herdr to read what a finished lane
// reported; Herdforge's own help used to send them there.
func runTranscript(args []string) {
	fs := flag.NewFlagSet("transcript", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	asJSON := fs.Bool("json", false, "emit the transcript as JSON")
	lines := fs.Int("lines", 0, "how many recent lines to read (default 200)")
	handoffOnly := fs.Bool("handoff", false, "print only the lane's final reported block")
	// Accept the agent name before or after flags. Go's flag package stops at
	// the first non-flag argument, so `transcript NAME --json` would otherwise
	// silently ignore --json.
	name := ""
	flags := make([]string, 0, len(args))
	for _, a := range args {
		if name == "" && !strings.HasPrefix(a, "-") && !wantsValue(flags) {
			name = a
			continue
		}
		flags = append(flags, a)
	}
	if err := fs.Parse(flags); err != nil {
		os.Exit(2)
	}
	if name == "" && fs.NArg() == 1 {
		name = fs.Arg(0)
	}
	if name == "" || fs.NArg() > 1 {
		fmt.Fprintln(os.Stderr, "usage: herd transcript <agent-name> [--handoff] [--lines N] [--json]")
		os.Exit(2)
	}

	result, err := transcript.Read(name, *lines)
	if err != nil {
		// A closed tab genuinely has no pane; say so rather than printing an
		// empty transcript that reads like a healthy silent lane.
		var missing *transcript.ErrNoSuchAgent
		if errors.As(err, &missing) {
			fmt.Fprintf(os.Stderr, "herd transcript: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(result); encErr != nil {
			fmt.Fprintf(os.Stderr, "herd transcript: encode: %v\n", encErr)
			os.Exit(1)
		}
		return
	}

	if *handoffOnly {
		if result.Handoff == "" {
			fmt.Fprintf(os.Stderr, "herd transcript: no reported block found for %s (status=%s)\n",
				result.Name, result.Status)
			os.Exit(1)
		}
		fmt.Println(result.Handoff)
		return
	}

	state := "working"
	if result.Finished {
		state = "finished"
	}
	fmt.Printf("agent %s (%s) status=%s %s pane=%s cwd=%s\n",
		result.Name, result.Kind, result.Status, state, result.PaneID, result.Cwd)
	if len(result.Commits) > 0 {
		fmt.Printf("candidate objects: %v\n", result.Commits)
	}
	if result.Handoff != "" {
		fmt.Printf("\n--- reported handoff ---\n%s\n", result.Handoff)
	}
	if result.Text != "" {
		fmt.Printf("\n--- recent output ---\n%s\n", result.Text)
	}
}

// wantsValue reports whether the last flag consumed expects a separate value,
// so a value like the N in "--lines N" is not mistaken for the agent name.
func wantsValue(flags []string) bool {
	if len(flags) == 0 {
		return false
	}
	last := flags[len(flags)-1]
	if strings.Contains(last, "=") || !strings.HasPrefix(last, "-") {
		return false
	}
	return strings.TrimLeft(last, "-") == "lines"
}
