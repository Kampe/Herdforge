package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	herdprocess "github.com/Kampe/Herdforge/pkg/process"
	"github.com/Kampe/Herdforge/pkg/quotasup"
	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// runQuotaSupervisor ports bin/herd-quota-supervisor: read live pool-specific
// usage BEFORE panes fail, map every live agent to the pool it actually runs
// on, and convert quota + cooldown + active-process evidence into a bounded
// per-surface concurrency cap and routing posture.
func runQuotaSupervisor() {
	fs := flag.NewFlagSet("quota-supervisor", flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Emit the capacity snapshot as JSON")
	readOnly := fs.Bool("read-only", false, "Observe only: no state write")
	act := fs.Bool("act", false, "Enforce blocked surfaces by writing bounded pool-scoped routing cooldowns")
	ttl := fs.Duration("act-ttl", quotasup.MaxActCooldown,
		fmt.Sprintf("Lifetime of a cool written by --act (clamped to [%s, %s])",
			quotasup.MinActCooldown, quotasup.MaxActCooldown))
	maxAge := fs.Duration("max-observation-age", quotasup.DefaultMaxObservationAge,
		"Quota readings older than this report UNKNOWN and authorize no work")
	warn := fs.Int("warn-runway", quotasup.DefaultWarnRunwayMinutes, "Minutes of runway that counts as at-risk")
	fs.Parse(os.Args[2:])

	// --act mutates the routing store that every launch consults. Silently
	// honouring it under --read-only would make the safest-looking invocation
	// the one with the widest blast radius.
	if *act && *readOnly {
		fmt.Fprintln(os.Stderr, "herd quota-supervisor: --act and --read-only are mutually exclusive")
		os.Exit(2)
	}

	stateFile := filepath.Join(".herd", "quota-supervisor.json")
	now := time.Now().UTC()

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
		ObservedAt:        now.Format(time.RFC3339),
		SourceAt:          snap.GeneratedAt.UTC().Format(time.RFC3339),
		Workspace:         workspace,
		WarnRunwayMinutes: *warn,
	}

	// Active-process evidence: what is running, and on which pool. The model
	// comes from the live process argv because `herdr agent list` does not
	// report one; without it every lane would be counted against its
	// provider's default pool.
	active := map[quotasup.Surface]int{}
	models := map[quotasup.Surface]map[string]bool{}
	providerErrors := map[quotasup.Surface][]string{}
	paneErrors := collectPaneProviderErrors(agents, workspace)
	for _, a := range agents {
		if a.Workspace != workspace || strings.TrimSpace(a.Name) == "" {
			continue
		}
		provider := a.Kind
		model, resolved := laneModel(a.PaneID)
		qp := quotasup.QuotaProvider(provider)
		pool := quotasup.QuotaPool(provider, model)
		assignment := quotasup.Assignment{
			Name: a.Name, PaneID: a.PaneID, TabID: a.TabID, AgentStatus: a.Status,
			Provider: provider, QuotaProvider: qp, Model: model, ModelResolved: resolved,
			Family: router.FamilyFor(strings.ToLower(provider), model), Pool: pool,
			Capacity: quotasup.Classify(quotasup.BurnFor(computed, quotasup.Surface{Provider: qp, Pool: pool}), *warn),
		}
		// Herdr status can remain "working" after a provider has stopped
		// accepting requests. Read the recent pane tail and treat an explicit
		// quota/rate-limit failure as authoritative live evidence.
		if reason := paneErrors[a.Name]; reason != "" {
			assignment.ProviderError = reason
			assignment.Capacity = quotasup.Exhausted
			providerErrors[assignment.Surface()] = append(providerErrors[assignment.Surface()], a.Name+": "+reason)
		}
		current.Agents = append(current.Agents, assignment)

		s := assignment.Surface()
		active[s]++
		if model != "" {
			if models[s] == nil {
				models[s] = map[string]bool{}
			}
			models[s][model] = true
		}
	}

	var prior *quotasup.Snapshot
	if raw, err := os.ReadFile(stateFile); err == nil {
		var p quotasup.Snapshot
		if json.Unmarshal(raw, &p) == nil {
			prior = &p
		}
	}

	live := make([]quotasup.Surface, 0, len(active))
	for s := range active {
		live = append(live, s)
	}
	for _, s := range quotasup.Surfaces(computed, live) {
		ev := quotasup.Observation{
			Surface:        s,
			Burn:           quotasup.BurnFor(computed, s),
			SourceAt:       snap.GeneratedAt,
			Cooldown:       quotasup.SurfaceCooldown(now, s),
			Active:         active[s],
			Models:         sortedKeys(models[s]),
			ProviderErrors: providerErrors[s],
		}.Grade(now, *warn, *maxAge)
		current.Decisions = append(current.Decisions,
			quotasup.Decide(s, ev, quotasup.PriorState(prior, s)))
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
	for _, d := range current.Decisions {
		if d.Cap != d.PriorCap {
			fmt.Printf("CAP_CHANGE surface=%s cap=%d->%d posture=%s. %s\n",
				d.Surface, d.PriorCap, d.Cap, d.Posture, d.Reason)
		}
	}

	if *act {
		changes, err := quotasup.Act(now, *ttl, current.Decisions)
		for _, c := range changes {
			fmt.Println("ACT " + c)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd quota-supervisor: %v\n", err)
			os.Exit(1)
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
	// Naming the guesses beats a clean-looking total: these lanes are counted
	// against their provider's default pool because their argv was unreadable.
	if unresolved := current.UnresolvedModels(); len(unresolved) > 0 {
		fmt.Printf("  WARN %d lane(s) billed to a default pool without argv evidence: %s\n",
			len(unresolved), strings.Join(unresolved, " "))
	}
	for _, d := range current.Decisions {
		fmt.Printf("  %-24s cap=%d/%d posture=%-7s active=%d  %s\n",
			d.Surface, d.Cap, d.Target, d.Posture, d.Evidence.Active, d.Reason)
	}
}

// collectPaneProviderErrors reads pane tails concurrently. A Chainseer
// workspace can have dozens of standing/reviewer panes; invoking the Herdr
// CLI serially made a quota sweep take longer than its operator timeout and
// caused the supervisor to miss the very stalls it was meant to catch.
func collectPaneProviderErrors(agents []herdr.AgentEntry, workspace string) map[string]string {
	type result struct{ name, reason string }
	jobs := make(chan herdr.AgentEntry)
	results := make(chan result, len(agents))
	const workers = 6
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for a := range jobs {
				if tail, err := herdr.PaneRead(a.PaneID, 80); err == nil {
					if reason := herdprocess.ProviderExhaustionReason(tail); reason != "" {
						results <- result{name: a.Name, reason: reason}
					}
				}
			}
		}()
	}
	go func() {
		for _, a := range agents {
			if a.Workspace == workspace && strings.TrimSpace(a.Name) != "" && strings.TrimSpace(a.PaneID) != "" {
				jobs <- a
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()
	out := make(map[string]string)
	for r := range results {
		out[r.name] = r.reason
	}
	return out
}

// laneModel recovers a running lane's model from its own process argv, the
// only live source for it.
//
// PaneProcessArgv, not PaneProcessInfo: herdr does not always report argv, and
// the OS read is what makes this evidence rather than a guess. Best effort by
// design — a pane that has already gone away, or an agent launched on its
// surface default, yields "" and bills the default pool rather than failing the
// whole sweep. resolved reports whether argv was actually read, so the caller
// can say which lanes are billed on evidence and which on a fallback.
func laneModel(paneID string) (model string, resolved bool) {
	if strings.TrimSpace(paneID) == "" {
		return "", false
	}
	procs, _ := herdr.PaneProcessArgv(paneID)
	for _, p := range procs {
		if len(p.Argv) == 0 {
			continue
		}
		resolved = true
		if m := quotasup.ModelFromArgv(p.Argv); m != "" {
			return m, true
		}
	}
	return "", resolved
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
