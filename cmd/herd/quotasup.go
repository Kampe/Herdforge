package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/quotasup"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// runQuotaSupervisor ports bin/herd-quota-supervisor: read live pool-specific
// usage BEFORE panes fail, map every live agent to the pool it actually runs
// on, classify capacity, and report transitions.
func runQuotaSupervisor() {
	fs := flag.NewFlagSet("quota-supervisor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Emit the capacity snapshot as JSON")
	readOnly := fs.Bool("read-only", false, "Observe only: no state write")
	warn := fs.Int("warn-runway", quotasup.DefaultWarnRunwayMinutes, "Minutes of runway that counts as at-risk")
	fs.Parse(os.Args[2:])

	stateFile := filepath.Join(".herd", "quota-supervisor.json")

	// Live quota is the authority. If it is unreadable, refuse — guessing
	// capacity is how work gets sent at a dead pool.
	engine := usage.NewQuotaEngine()
	snap, err := usage.FetchSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd quota-supervisor: live quota unreadable; refusing to guess: %v\n", err)
		os.Exit(1)
	}
	computed := engine.ComputeAll(snap)

	workspace, err := herdr.RequireWorkspace(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd quota-supervisor: workspace unresolved: %v\n", err)
		os.Exit(1)
	}
	agents, err := herdr.AgentList()
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd quota-supervisor: agent roster unreadable: %v\n", err)
		os.Exit(1)
	}

	current := &quotasup.Snapshot{
		ObservedAt:        time.Now().UTC().Format(time.RFC3339),
		Workspace:         workspace,
		WarnRunwayMinutes: *warn,
	}
	for _, a := range agents {
		if a.Workspace != workspace || strings.TrimSpace(a.Name) == "" {
			continue
		}
		provider := a.Kind
		qp := quotasup.QuotaProvider(provider)
		pool := quotasup.QuotaPool(provider, "")
		var st *usage.BurnState
		if burn, ok := computed[qp]; ok {
			if p, ok := burn.Pools[pool]; ok {
				st = &p
			} else {
				st = &burn
			}
		}
		current.Agents = append(current.Agents, quotasup.Assignment{
			Name: a.Name, PaneID: a.PaneID, TabID: a.TabID, AgentStatus: a.Status,
			Provider: provider, QuotaProvider: qp, Pool: pool,
			Capacity: quotasup.Classify(st, *warn),
		})
	}

	var prior *quotasup.Snapshot
	if raw, err := os.ReadFile(stateFile); err == nil {
		var p quotasup.Snapshot
		if json.Unmarshal(raw, &p) == nil {
			prior = &p
		}
	}
	for _, a := range current.Agents {
		old := quotasup.Prior(prior, a.Name)
		if quotasup.IsTransition(old, a.Capacity) {
			fmt.Printf("QUOTA_CHANGE lane=%s provider=%s pool=%s capacity=%s->%s. "+
				"Reprioritize now: preserve in-flight work; send no new work to exhausted or at-risk pools; "+
				"move the highest-value ready work to verified healthy pools.\n",
				a.Name, a.Provider, a.Pool, old, a.Capacity)
		}
	}

	// A status glance that mkdirs and rewrites state is a mutation, not an
	// observation. --read-only must leave the filesystem untouched.
	if !*readOnly {
		if err := os.MkdirAll(filepath.Dir(stateFile), 0o700); err == nil {
			if body, err := json.Marshal(current); err == nil {
				tmp := stateFile + ".tmp"
				if os.WriteFile(tmp, body, 0o600) == nil {
					os.Rename(tmp, stateFile)
				}
			}
		}
	}

	if *asJSON {
		body, _ := json.MarshalIndent(current, "", "  ")
		fmt.Println(string(body))
		return
	}
	c := current.Counts()
	fmt.Printf("herd quota-supervisor: agents=%d exhausted=%d at_risk=%d unknown=%d\n",
		c.Agents, c.Exhausted, c.AtRisk, c.Unknown)
	for name, burn := range computed {
		fmt.Printf("  pool %s: used=%.0f%% remaining=%.0f%% class=%s\n",
			name, burn.Used, burn.Remaining, burn.Class)
	}
}
