package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/Kampe/Herdforge/pkg/boardfreeze"
)

// runBoardFreeze ports bin/herd-board-frozen into a durable gate (FAC-103):
// every provider mutation adapter (pkg/provider.BoundClient) refuses before
// the underlying write while this gate is on. Unlike `herd board-frozen`
// (a simple env/file trigger check), this command persists who froze the
// board, why, for what scope, and for how long, and every refusal is
// counted so `status` can show pending blocked mutations.
func runBoardFreeze() {
	fs := flag.NewFlagSet("board-freeze", flag.ExitOnError)
	fs.Parse(os.Args[2:])
	action := "status"
	if fs.NArg() > 0 {
		action = fs.Arg(0)
	}

	switch action {
	case "on":
		onFs := flag.NewFlagSet("board-freeze on", flag.ExitOnError)
		actor := onFs.String("actor", "", "who is freezing the board (required)")
		reason := onFs.String("reason", "", "why the board is being frozen (required)")
		scope := onFs.String("scope", "", "optional scope this freeze applies to (e.g. a repo or lane)")
		expires := onFs.String("expires", "", "optional expiry: a duration (e.g. 2h) or RFC3339 timestamp")
		onFs.Parse(fs.Args()[1:])
		if *actor == "" || *reason == "" {
			fmt.Fprintln(os.Stderr, "herd board-freeze on: --actor and --reason are required")
			os.Exit(2)
		}
		var expiresAt *time.Time
		if *expires != "" {
			t, err := parseExpiry(*expires, time.Now())
			if err != nil {
				fmt.Fprintf(os.Stderr, "herd board-freeze on: --expires: %v\n", err)
				os.Exit(2)
			}
			expiresAt = &t
		}
		st, err := boardfreeze.SetState(true, *actor, *reason, *scope, expiresAt, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd board-freeze on: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("herd board-freeze: ON (generation %d, actor=%s, reason=%q)\n", st.Generation, st.Actor, st.Reason)
		fmt.Println("  every provider mutation (status/comment/claim/label/relation) now refuses before writing")

	case "off":
		offFs := flag.NewFlagSet("board-freeze off", flag.ExitOnError)
		actor := offFs.String("actor", "", "who is clearing the freeze (required)")
		reason := offFs.String("reason", "", "optional note on why the freeze is being cleared")
		offFs.Parse(fs.Args()[1:])
		if *actor == "" {
			fmt.Fprintln(os.Stderr, "herd board-freeze off: --actor is required")
			os.Exit(2)
		}
		st, err := boardfreeze.SetState(false, *actor, *reason, "", nil, time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd board-freeze off: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("herd board-freeze: OFF (generation %d, actor=%s)\n", st.Generation, st.Actor)
		fmt.Println("  provider mutations resume")

	case "status":
		st, frozen, err := boardfreeze.Active(time.Now())
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd board-freeze status: %v (FAIL CLOSED: treat as frozen)\n", err)
			os.Exit(1)
		}
		effective := "OFF"
		if frozen {
			effective = "ON"
		}
		fmt.Printf("herd board-freeze: sentinel=%v EFFECTIVE=%s (generation %d)\n", st.On, effective, st.Generation)
		if st.On {
			fmt.Printf("  actor=%s reason=%q scope=%q\n", st.Actor, st.Reason, st.Scope)
			if st.ExpiresAt != nil {
				fmt.Printf("  expires=%s (expired=%v)\n", st.ExpiresAt.Format(time.RFC3339), st.Expired(time.Now()))
			}
		}
		fmt.Printf("  changed_at=%s\n", st.ChangedAt.Format(time.RFC3339))
		fmt.Printf("  blocked mutations since gate created: %d\n", st.BlockedMutations)

	case "-h", "--help":
		fmt.Println("Usage: herd board-freeze [status|on|off]")
		fmt.Println("  on  --actor NAME --reason TEXT [--scope TEXT] [--expires 2h|RFC3339]")
		fmt.Println("  off --actor NAME [--reason TEXT]")

	default:
		fmt.Fprintf(os.Stderr, "herd board-freeze: unknown action %q (status|on|off)\n", action)
		os.Exit(2)
	}
}

// parseExpiry accepts either a duration relative to now (e.g. "2h30m") or
// an absolute RFC3339 timestamp.
func parseExpiry(raw string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		return now.Add(d), nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a duration or RFC3339 timestamp: %q", raw)
	}
	return t, nil
}
