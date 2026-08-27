package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/provider"
)

// runDeps is the FAC-159 dependency-graph conformance CLI.
//
//	herd deps selftest          — FAC-75/90/93/105 fixtures + mutation controls
//	herd deps check <ref>       — pre-side-effect gate for one ref (no mutations)
//	herd deps reconcile <ref>   — packet desired vs board (JSON)
//
// Exit 0 on clean, 1 on BLOCKED/drift/capability/error.
func runDeps() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, `usage: herd deps <selftest|check|reconcile|migrate> [args]
  selftest           Run FAC-75/90/93/105 drift fixtures + mutation controls
  check <ref>        Pre-side-effect launch gate for ref (read-only)
  reconcile <ref>    Emit stable JSON reconcile report for ref
  migrate            Revision-fenced dry-run (default); --apply is coordinator-only`)
		os.Exit(2)
	}
	switch os.Args[2] {
	case "selftest":
		runDepsSelftest()
	case "check":
		runDepsCheck()
	case "reconcile":
		runDepsReconcile()
	case "migrate":
		runDepsMigrate()
	case "--help", "-h", "help":
		fmt.Println(`herd deps — packet↔board dependency-graph conformance (FAC-159)

Commands:
  selftest           Hermetic FAC-75/90/93/105 drift fixtures + mutation controls
  check <ref>        Validate launch eligibility without side effects
  reconcile <ref>    Print stable JSON (missing/extra/duplicate/reversed/...)
  migrate            Dry-run description fence plan; --apply requires HERD_DEPS_MIGRATE_APPLY=1 (coordinator)`)
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "herd deps: unknown subcommand %q\n", os.Args[2])
		os.Exit(2)
	}
}

func runDepsSelftest() {
	fs := flag.NewFlagSet("deps selftest", flag.ExitOnError)
	_ = fs.Parse(os.Args[3:])

	fmt.Println("herd deps selftest: FAC-75/90/93/105 drift fixture")
	if err := deps.RunFixture(deps.FAC759093105Fixture()); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL fixture: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  fixture: PASS")

	if err := deps.MutationControl_ReconcileRemoved(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL mutation control reconcile: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  mutation reconcile: PASS")

	if err := deps.MutationControl_GateBypassed(); err != nil {
		fmt.Fprintf(os.Stderr, "FAIL mutation control gate: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("  mutation gate: PASS")

	// Prove production entrypoint constants exist (reachability surface).
	entries := []deps.LaunchEntrypoint{
		deps.EntryDispatch, deps.EntryPulse, deps.EntryWave, deps.EntryStanding,
		deps.EntryShot, deps.EntryRescue, deps.EntryRecovery, deps.EntryForge, deps.EntryClaim,
	}
	if len(entries) < 7 {
		fmt.Fprintln(os.Stderr, "FAIL entrypoint surface incomplete")
		os.Exit(1)
	}
	fmt.Printf("  entrypoints: %d registered\n", len(entries))
	fmt.Println("herd deps selftest: PASS")
}

func runDepsCheck() {
	fs := flag.NewFlagSet("deps check", flag.ExitOnError)
	entry := fs.String("entry", "dispatch", "launch entrypoint label")
	_ = fs.Parse(os.Args[3:])
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: herd deps check <ref>")
		os.Exit(2)
	}
	ref := fs.Arg(0)

	cfg, err := config.LoadConfig(config.DefaultConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "provider: %v\n", tpErr)
		os.Exit(1)
	}
	store := deps.StoreFor(tp, cfg.TaskProvider.ProjectID)

	// Load desired from task description fence if present (never free-text).
	// Extract errors are hard failures (never ignored).
	task, gerr := tp.GetTask(context.Background(), ref)
	if gerr != nil || task == nil {
		fmt.Fprintf(os.Stderr, "task %s: %v\n", ref, gerr)
		os.Exit(1)
	}
	desired, xerr := deps.ExtractProvenanceFromText(task.Description)
	if xerr != nil {
		fmt.Fprintf(os.Stderr, "provenance extract: %v\n", xerr)
		os.Exit(1)
	}
	// Vacuity lives inside ValidateLaunch on the scoped snapshot. Do not
	// SnapshotGraph here: that was the whole-project read that timed out
	// exact-card admission before RequireTaskLaunch could use SnapshotGraphForTask.
	ep := deps.LaunchEntrypoint(*entry)
	gr, err := deps.RequireTaskLaunch(context.Background(), store, ep, deps.Ref(ref), desired, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "BLOCKED %s: %v\n", ref, err)
		if gr != nil && gr.Report != nil {
			b, _ := deps.MarshalReport(gr.Report)
			fmt.Println(string(b))
		}
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(gr, "", "  ")
	fmt.Println(string(out))
}

func runDepsReconcile() {
	fs := flag.NewFlagSet("deps reconcile", flag.ExitOnError)
	_ = fs.Parse(os.Args[3:])
	if fs.NArg() < 1 {
		fmt.Fprintln(os.Stderr, "usage: herd deps reconcile <ref>")
		os.Exit(2)
	}
	ref := fs.Arg(0)

	cfg, err := config.LoadConfig(config.DefaultConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "provider: %v\n", tpErr)
		os.Exit(1)
	}
	store := deps.StoreFor(tp, cfg.TaskProvider.ProjectID)
	task, gerr := tp.GetTask(context.Background(), ref)
	if gerr != nil || task == nil {
		fmt.Fprintf(os.Stderr, "task %s: %v\n", ref, gerr)
		os.Exit(1)
	}
	desired, xerr := deps.ExtractProvenanceFromText(task.Description)
	if xerr != nil {
		fmt.Fprintf(os.Stderr, "provenance extract: %v\n", xerr)
		os.Exit(1)
	}
	var desEdges []deps.DependencyEdge
	if desired != nil && desired.Present {
		var derr error
		desEdges, derr = desired.DesiredBlocks()
		if derr != nil {
			fmt.Fprintf(os.Stderr, "provenance: %v\n", derr)
			os.Exit(1)
		}
	}
	// Operator reconcile asks for project-wide cycle proof (RequireFullClosure
	// + FullClosure). That is not the launch eligibility path: ValidateLaunch
	// already scopes via SnapshotGraphForTask. Keep the bulk read here.
	snap, lerr := store.SnapshotGraph(context.Background())
	if lerr != nil {
		fmt.Fprintf(os.Stderr, "snapshot graph: %v\n", lerr)
		os.Exit(1)
	}
	board := snap.Edges
	rep := deps.Reconcile(deps.Ref(ref), desEdges, board, deps.ReconcileOpts{
		FullClosure:        snap.Edges,
		ProviderRevision:   snap.ProviderRevision,
		RequireFullClosure: true,
	})
	b, err := deps.MarshalReport(rep)
	if err != nil {
		fmt.Fprintf(os.Stderr, "marshal: %v\n", err)
		os.Exit(1)
	}
	fmt.Println(string(b))
	if !rep.OK {
		os.Exit(1)
	}
}

func runDepsMigrate() {
	fs := flag.NewFlagSet("deps migrate", flag.ExitOnError)
	apply := fs.Bool("apply", false, "Coordinator-only: write description fences (requires HERD_DEPS_MIGRATE_APPLY=1)")
	asJSON := fs.Bool("json", false, "JSON plan output")
	journalDir := fs.String("journal", filepath.Join(".herd", "migrate-journal"), "Per-apply before-image journal dir (repo-relative)")
	_ = fs.Parse(os.Args[3:])

	cfg, err := config.LoadConfig(config.DefaultConfigPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config: %v\n", err)
		os.Exit(1)
	}
	tp, tpErr := loadTaskProvider(cfg)
	if tpErr != nil {
		fmt.Fprintf(os.Stderr, "provider: %v\n", tpErr)
		os.Exit(1)
	}
	store := deps.StoreFor(tp, cfg.TaskProvider.ProjectID)
	ctx := context.Background()

	// Default path: revision-fenced dry-run. Workers ship this; they never apply live.
	if !*apply {
		totalCards, processedCards := 0, 0
		plan, perr := deps.PlanMigrationWithProgress(ctx, store, tp, cfg.TaskProvider.ProjectID, func(item deps.MigrateItem, processed, total int) {
			totalCards, processedCards = total, processed
			fmt.Fprintf(os.Stderr, "herd deps migrate dry-run: processed %d/%d cards (current=%s action=%s)\n", processed, total, item.Ref, item.Action)
		})
		if perr != nil {
			fmt.Fprintf(os.Stderr, "migrate dry-run: partial progress migrated=%d remaining=%d: %v\n", processedCards, totalCards-processedCards, perr)
			if *asJSON && plan != nil {
				b, _ := json.MarshalIndent(plan, "", "  ")
				fmt.Println(string(b))
			}
			os.Exit(1)
		}
		if *asJSON {
			b, _ := json.MarshalIndent(plan, "", "  ")
			fmt.Println(string(b))
		} else {
			fmt.Printf("herd deps migrate dry-run: %d active tasks (description fences + Kaneo relations only)\n", len(plan.Items))
			for _, it := range plan.Items {
				fmt.Printf("  %s action=%s edges=%d %s\n", it.Ref, it.Action, it.EdgeCount, it.Detail)
			}
			if !plan.OK {
				os.Exit(1)
			}
		}
		return
	}

	// Coordinator-run apply only. Sequence after candidate review PASS:
	//   dry-run at exact SHA → HERD_DEPS_MIGRATE_APPLY=1 migrate --apply →
	//   per-card journal + multiset readback → then merge/enable gate.
	// Workers must never set the env gate against live Kaneo.
	if os.Getenv("HERD_DEPS_MIGRATE_APPLY") != "1" {
		fmt.Fprintln(os.Stderr, `herd deps migrate --apply refused: coordinator-only
set HERD_DEPS_MIGRATE_APPLY=1 after review PASS + revision-fenced dry-run
workers write/test this command; they never apply live (no sidecar authority)`)
		os.Exit(1)
	}

	var writer deps.DescriptionWriter
	switch strings.ToLower(strings.TrimSpace(cfg.TaskProvider.Type)) {
	case "memory":
		mp, ok := tp.(*provider.MemoryProvider)
		if !ok {
			fmt.Fprintln(os.Stderr, "migrate apply: memory provider type mismatch")
			os.Exit(1)
		}
		writer = deps.MemoryDescriptionWriter{MP: mp}
	case "kaneo":
		// Description fences are the only write surface (no unsigned sidecar).
		writer = deps.KaneoDescriptionWriter{ProjectID: cfg.TaskProvider.ProjectID}
	default:
		fmt.Fprintf(os.Stderr, "migrate apply: provider %q has no DescriptionWriter (description fences only)\n", cfg.TaskProvider.Type)
		os.Exit(1)
	}

	plan, perr := deps.ApplyMigration(ctx, store, tp, cfg.TaskProvider.ProjectID, writer, *journalDir)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "migrate apply: %v\n", perr)
		os.Exit(1)
	}
	if *asJSON {
		b, _ := json.MarshalIndent(plan, "", "  ")
		fmt.Println(string(b))
	} else {
		fmt.Printf("herd deps migrate apply-description: %d items journal=%s\n", len(plan.Items), plan.JournalPath)
		for _, it := range plan.Items {
			fmt.Printf("  %s action=%s applied=%v readback=%v rolled_back=%v %s\n",
				it.Ref, it.Action, it.Applied, it.ReadbackOK, it.RolledBack, it.Detail)
		}
	}
	if !plan.OK {
		os.Exit(1)
	}
}
