package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/Kampe/Herdforge/pkg/park"
)

func runPark() {
	if len(os.Args) < 3 {
		parkUsage()
		os.Exit(1)
	}

	sub := os.Args[2]
	ctx := context.Background()

	repoRoot, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "park: %v\n", err)
		os.Exit(1)
	}

	switch sub {
	case "park":
		// Manual parse: the flag package stops at the first non-flag
		// argument, but the spec's grammar is "<slug> <sha> -m <message>"
		// — positionals before the flag.
		var msg string
		signFirst := true
		var positional []string
		rest := os.Args[3:]
		for i := 0; i < len(rest); i++ {
			switch rest[i] {
			case "-m":
				if i+1 >= len(rest) {
					fmt.Fprintf(os.Stderr, "Usage: herd park park <slug> <sha> -m <message>\n")
					os.Exit(2)
				}
				i++
				msg = rest[i]
			case "--no-sign-first":
				signFirst = false
			default:
				positional = append(positional, rest[i])
			}
		}
		if len(positional) < 2 {
			fmt.Fprintf(os.Stderr, "Usage: herd park park <slug> <sha> -m <message>\n")
			os.Exit(2)
		}
		slug, sha := positional[0], positional[1]

		res, err := park.Park(ctx, park.ParkOptions{RepoRoot: repoRoot, SignFirst: signFirst}, slug, sha, msg)
		if res != nil && res.SignWarning != "" {
			fmt.Fprintf(os.Stderr, "herd-park: WARN %s\n", res.SignWarning)
		}
		switch {
		case err == nil:
			suffix := ""
			if !res.Signed {
				suffix = ", but UNSIGNED (see the warning above)"
			}
			fmt.Fprintf(os.Stderr, "herd-park: %s -> %s parked DURABLY (annotated tag pushed to origin).%s\n", res.Tag, res.ShortSHA, suffix)
			writeJSONOrDie(res)
		case errors.Is(err, park.ErrNotCommit), errors.Is(err, park.ErrMessageRequired):
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		case errors.Is(err, park.ErrPushFailed):
			fmt.Fprintln(os.Stderr, err)
			writeJSONOrDie(res)
			os.Exit(1)
		default:
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}

	case "list":
		flags := flag.NewFlagSet("park list", flag.ExitOnError)
		wantJSON := flags.Bool("json", false, "Output JSON")
		flags.Parse(os.Args[3:])

		result, err := park.List(ctx, repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "park list: %v\n", err)
			os.Exit(1)
		}
		if *wantJSON {
			writeJSONOrDie(result)
			return
		}
		for _, c := range result.Commits {
			fmt.Printf("%s\t%s\t%s\n", c.Tag, c.Commit, c.Message)
		}

	case "audit":
		flags := flag.NewFlagSet("park audit", flag.ExitOnError)
		quiet := flags.Bool("quiet", false, "Suppress the table, print only the summary/exit")
		flags.Parse(os.Args[3:])

		result, err := park.Audit(ctx, repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "park audit: %v\n", err)
			os.Exit(1)
		}
		if !*quiet {
			for _, e := range result.Entries {
				fmt.Printf("%-14s  %s  %-30s  %s\n", e.Durability, e.SHA[:min(7, len(e.SHA))], e.Branch, e.Subject)
			}
		}
		if !park.VerifyAuditExit(result) {
			fmt.Fprintf(os.Stderr, "herd-park: %d parked commit(s) are NOT durable.\n", result.NotDurable)
			fmt.Fprintln(os.Stderr, "  orphaned by reset --hard (agent-preflight --confirm-merged after every harvest), then gc'd.")
			fmt.Fprintln(os.Stderr, "  fix: bin/herd-park <slug> <sha> -m \"...\"")
			os.Exit(1)
		}
		fmt.Println("herd-park: all parked work is durable (annotated tag pushed to origin).")

	case "hygiene":
		flags := flag.NewFlagSet("park hygiene", flag.ExitOnError)
		quiet := flags.Bool("quiet", false, "Suppress the table, print only the summary/exit")
		flags.Parse(os.Args[3:])

		result, err := park.Hygiene(ctx, repoRoot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "park hygiene: %v\n", err)
			os.Exit(1)
		}
		if !*quiet {
			for _, r := range result.Rows {
				fmt.Printf("%-16s  %-30s  %s  %s  %s\n", r.Flag, r.Branch, r.SHA, r.Age, r.Subject)
			}
			for _, c := range result.DupClusters {
				fmt.Printf("DUP %s\n", c)
			}
		}
		fmt.Printf("herd-park hygiene: %d multi-tip CHA cluster(s), %d content-merged tip(s).\n", len(result.DupClusters), result.ContentMerged)
		if !park.VerifyHygieneExit(result) {
			os.Exit(1)
		}

	case "reap":
		flags := flag.NewFlagSet("park reap", flag.ExitOnError)
		apply := flags.Bool("apply", false, "Actually delete via bin/herd-gc (default: dry run)")
		flags.Parse(os.Args[3:])

		res, err := park.Reap(ctx, repoRoot, *apply)
		if err != nil {
			if errors.Is(err, park.ErrGCNotFound) {
				fmt.Fprintf(os.Stderr, "park reap: bin/herd-gc not found\n")
				os.Exit(3)
			}
			fmt.Fprintf(os.Stderr, "park reap: %v\n", err)
			os.Exit(1)
		}
		if !*apply {
			fmt.Println("DRY RUN. Re-run with --apply to delete")
		}
		writeJSONOrDie(res)

	default:
		parkUsage()
		os.Exit(1)
	}
}

// writeJSONOrDie encodes v to stdout and exits 1 on failure (e.g. a broken
// pipe) instead of silently reporting success on a lost write.
func writeJSONOrDie(v any) {
	if err := json.NewEncoder(os.Stdout).Encode(v); err != nil {
		fmt.Fprintf(os.Stderr, "park: failed to write output: %v\n", err)
		os.Exit(1)
	}
}

func parkUsage() {
	fmt.Fprintf(os.Stderr, `park: durable annotated tag parking in refs/tags/parked/

Subcommands:
  park <slug> <sha> -m <msg>   Tag <sha> as refs/tags/parked/<slug> and force-push it
  list                         List parked tags
  audit [--quiet]              Classify wip(parked)/wip:/parked: branch commits by durability
  hygiene [--quiet]            Classify park/parked branch tips by merge status and CHA dup
  reap [--apply]     Delegate branch cleanup to bin/herd-gc (dry run by default)
`)
}
