package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/watch"
)

// runWatch ports bin/herd-watch: fire the moment a builder or reviewer leaves
// `working`, so the coordinator harvests without polling by hand.
func runWatch() {
	fs := flag.NewFlagSet("watch", flag.ExitOnError)
	stream := fs.Bool("stream", false, "Print one line per settle forever (harvest trigger feed)")
	all := fs.Bool("all", false, "Fire only when every named pane has settled")
	intervalSec := fs.Int("interval", int(watch.DefaultInterval.Seconds()), "Seconds between polls")
	timeoutSec := fs.Int("timeout", 14400, "Give up after this many seconds")
	fs.Parse(os.Args[2:])

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
