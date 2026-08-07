package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Kampe/Herdforge/pkg/cmdsession"
)

func commandSessionDBPath(dbFlag string) string {
	if dbFlag != "" {
		return dbFlag
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	return filepath.Join(root, ".herd", "command-sessions.db")
}

func runCommandSessions() {
	if len(os.Args) > 2 && os.Args[2] == "reconcile" {
		runCommandSessionsReconcile(os.Args[3:])
		return
	}
	runCommandSessionsStatus(os.Args[2:])
}

func runCommandSessionsStatus(args []string) {
	fs := flag.NewFlagSet("commands", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output the JSON summary")
	dbFlag := fs.String("db", "", "Path to the command session receipt DB (default $HERD_ROOT/.herd/command-sessions.db)")
	fs.Usage = func() {
		fmt.Println(`herd-commands , retained command session status (FAC-193):
  every exec session, PTY, shell, and bounded CLI child a coordinator or
  worker command runner started that is not yet accounted for, with its
  age, task, and disposition — so a finished tool call cannot leave a
  hidden background terminal behind an agent-level working state.

    herd commands                     # human summary
    herd commands --json              # machine-readable summary
    herd commands reconcile           # dry run: what a sweep would decide
    herd commands reconcile --apply   # settle absent sessions, record BLOCKED evidence`)
	}
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fs.Usage()
			os.Exit(0)
		}
	}
	if err := fs.Parse(args); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-commands: %v\n", err)
		}
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "herd-commands: unknown arg %s\n", fs.Arg(0))
		os.Exit(2)
	}

	store, err := cmdsession.NewStore(commandSessionDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-commands: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	sum, err := store.Summarize(time.Now)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-commands: %v\n", err)
		os.Exit(1)
	}
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sum); err != nil {
			fmt.Fprintf(os.Stderr, "herd-commands: encode summary: %v\n", err)
			os.Exit(1)
		}
		return
	}
	printCommandSessionSummary(sum)
}

func printCommandSessionSummary(sum cmdsession.Summary) {
	fmt.Printf("retained command sessions: %d, blocked: %d, oldest: %s\n",
		sum.Retained, sum.Blocked, (time.Duration(sum.OldestAgeSeconds) * time.Second).String())
	for _, row := range sum.Rows {
		age := (time.Duration(row.AgeSeconds) * time.Second).String()
		fmt.Printf("  %-10s pid=%-7d age=%-10s task=%-10s %s\n", row.State, row.PID, age, row.TaskRef, row.Key)
		if row.DetachedOpen > 0 {
			fmt.Printf("      detached descendants still unaccounted for: %d\n", row.DetachedOpen)
		}
		if row.BlockedReason != "" {
			fmt.Printf("      BLOCKED: %s\n", row.BlockedReason)
		}
	}
}

func runCommandSessionsReconcile(args []string) {
	fs := flag.NewFlagSet("commands reconcile", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output the JSON report")
	dbFlag := fs.String("db", "", "Path to the command session receipt DB (default $HERD_ROOT/.herd/command-sessions.db)")
	apply := fs.Bool("apply", false, "Actually settle receipts and record BLOCKED evidence (default is a dry run)")
	fs.Usage = func() {
		fmt.Println(`herd-commands reconcile , coordinator recovery sweep (FAC-193):
  re-proves each retained session's exact identity (pid + start token +
  parentage) and disposes of it: sessions whose process is provably gone
  are settled, live sessions are preserved untouched, and anything
  ambiguous becomes durable BLOCKED evidence.

  This CLI runs outside the coordinator, so it holds no descriptors and
  is nobody's parent: it can never close or wait a session, and it never
  claims to have. A retained terminal that is still alive is reported
  BLOCKED for its owning process to reap. Default is a dry run.

    herd commands reconcile           # dry run
    herd commands reconcile --apply   # write the dispositions`)
	}
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fs.Usage()
			os.Exit(0)
		}
	}
	if err := fs.Parse(args); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-commands reconcile: %v\n", err)
		}
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "herd-commands reconcile: unknown arg %s\n", fs.Arg(0))
		os.Exit(2)
	}

	store, err := cmdsession.NewStore(commandSessionDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-commands reconcile: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	if !*apply {
		sum, err := store.Summarize(time.Now)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-commands reconcile: %v\n", err)
			os.Exit(1)
		}
		if *jsonFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(sum); err != nil {
				fmt.Fprintf(os.Stderr, "herd-commands reconcile: encode summary: %v\n", err)
				os.Exit(1)
			}
			return
		}
		fmt.Printf("dry run: %d retained session(s) would be re-proved (pass --apply to write dispositions)\n", sum.Retained)
		printCommandSessionSummary(sum)
		return
	}

	rep, err := cmdsession.Reconcile(store, cmdsession.SystemProbe, cmdsession.ForeignCloser, cmdsession.ForeignWaiter)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-commands reconcile: %v\n", err)
		os.Exit(1)
	}
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(rep); err != nil {
			fmt.Fprintf(os.Stderr, "herd-commands reconcile: encode report: %v\n", err)
			os.Exit(1)
		}
		return
	}
	fmt.Printf("settled: %d, preserved (live): %d, deferred (detached): %d, blocked: %d, reaped: %d\n",
		len(rep.Settled), len(rep.Preserved), len(rep.Deferred), len(rep.Blocked), len(rep.Reaped))
	for _, key := range rep.Blocked {
		fmt.Printf("  BLOCKED (needs its owning process to reap): %s\n", key)
	}
}

// commandSessionStatusLine is the retained-command-session line `herd status`
// prints. It reads an existing receipt DB only: `herd status` must never be
// the thing that creates one, or every read would report a fresh empty store
// as authoritative evidence of zero background terminals.
func commandSessionStatusLine(dbPath string, now func() time.Time) (string, error) {
	if _, err := os.Stat(dbPath); err != nil {
		if os.IsNotExist(err) {
			return "Retained command sessions: no receipt store yet", nil
		}
		return "", err
	}
	store, err := cmdsession.NewStore(dbPath)
	if err != nil {
		return "", err
	}
	defer store.Close()
	sum, err := store.Summarize(now)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("Retained command sessions: %d (blocked: %d, oldest: %s)",
		sum.Retained, sum.Blocked, (time.Duration(sum.OldestAgeSeconds) * time.Second).String()), nil
}
