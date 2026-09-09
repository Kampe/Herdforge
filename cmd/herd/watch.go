package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mail"
	"github.com/Kampe/Herdforge/pkg/watch"
)

// runWatch ports bin/herd-watch: fire the moment a builder or reviewer leaves
// `working`, so the coordinator harvests without polling by hand.
func runWatch() {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	stream := fs.Bool("stream", false, "Print one line per settle forever (harvest trigger feed)")
	all := fs.Bool("all", false, "Fire only when every named pane has settled")
	wake := fs.Bool("wake", false, "Reconcile ordinary durable mail for one exact idle/done recipient")
	recipient := fs.String("recipient", "", "Exact recipient for --wake")
	workspace := fs.String("workspace", "", "Exact Herdr workspace for --wake")
	mailOverride := fs.String("mail", "", "mailbox path override for --wake")
	intervalSec := fs.Int("interval", int(watch.DefaultInterval.Seconds()), "Seconds between polls")
	timeoutSec := fs.Int("timeout", 14400, "Give up after this many seconds")
	fs.Parse(os.Args[2:])
	if *wake && (strings.TrimSpace(*recipient) == "" || strings.TrimSpace(*workspace) == "") {
		fmt.Fprintln(os.Stderr, "herd watch: --wake requires exact --recipient and --workspace")
		os.Exit(2)
	}

	named := fs.Args()
	interval := time.Duration(*intervalSec) * time.Second
	deadline := time.Now().Add(time.Duration(*timeoutSec) * time.Second)
	state := watch.NewState()

	for {
		if time.Now().After(deadline) {
			fmt.Println("TIMEOUT")
			os.Exit(2)
		}

		// Re-enumerate EVERY poll. A fixed pane list drops reviewers spawned
		// mid-wave, and they then settle unnoticed.
		agents, err := herdr.AgentList()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd watch: agent list: %v\n", err)
			time.Sleep(interval)
			continue
		}
		var obs []watch.Observation
		attention := 0
		for _, a := range agents {
			if a.PaneID == "" {
				continue
			}
			if len(named) > 0 && !contains(named, a.PaneID) {
				continue
			}
			obs = append(obs, watch.Observation{PaneID: a.PaneID, Name: a.Name, Status: a.Status})
			if watch.Settled(a.Status) {
				attention++
			}
		}

		if *wake {
			mailPath, mailErr := controlMailPath(*mailOverride)
			if mailErr != nil {
				fmt.Fprintf(os.Stderr, "herd watch: wake mailbox: %v\n", mailErr)
				os.Exit(1)
			}
			if err := os.MkdirAll(filepath.Dir(mailPath), 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "herd watch: wake mailbox: %v\n", err)
				os.Exit(1)
			}
			occupied, wakeErr := surfaceQueuedAtKickMailbox(*recipient, *workspace, mail.NewMailbox(mailPath))
			if wakeErr != nil && !errors.Is(wakeErr, herdr.ErrNotIdleBoundary) {
				fmt.Fprintf(os.Stderr, "herd watch: wake: %v\n", wakeErr)
				os.Exit(1)
			}
			if occupied {
				fmt.Printf("WAKE %s workspace=%s durable mail consumed\n", *recipient, *workspace)
				if !*stream {
					return
				}
			}
		}

		events := state.Poll(obs)
		for _, e := range events {
			fmt.Println(watch.SettleLine(e, attention))
		}

		if !*stream {
			if *all && len(named) > 0 {
				if state.AllSettled(named) {
					return
				}
			} else if len(events) > 0 {
				return
			}
		}
		time.Sleep(interval)
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
