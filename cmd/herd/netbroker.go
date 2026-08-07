package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/security"
)

// runNetbrokerServe is the durable proxy process entry (FAC-133).
// Launched detached by security.StartDurableBroker; state file is the ready signal.
// --control holds coordinator-only control material (never agent-visible).
func runNetbrokerServe() {
	fs := flag.NewFlagSet("netbroker-serve", flag.ExitOnError)
	state := fs.String("state", "", "agent-visible proxy state json path")
	control := fs.String("control", "", "coordinator-only control state path")
	tab := fs.String("tab", "", "tab id")
	session := fs.String("session", "", "session id")
	allow := fs.String("allow", "", "comma-separated allow hosts")
	_ = fs.Parse(os.Args[2:])
	if *state == "" || *tab == "" {
		fmt.Fprintln(os.Stderr, "netbroker-serve: --state and --tab required")
		os.Exit(1)
	}
	if err := security.RunNetbrokerServe(*state, *control, *tab, *session, *allow); err != nil {
		fmt.Fprintf(os.Stderr, "netbroker-serve: %v\n", err)
		os.Exit(1)
	}
}
