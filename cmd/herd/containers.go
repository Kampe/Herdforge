package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/containerlifecycle"
)

// containerListerFunc is a package-level seam so tests can fake the live
// docker audit without touching a real daemon, matching the exec-seam
// convention used elsewhere in cmd/herd.
var containerListerFunc containerlifecycle.ContainerLister = containerlifecycle.DockerListAll

func runContainers() {
	if len(os.Args) > 2 && os.Args[2] == "reconcile" {
		runContainersReconcile(os.Args[3:])
		return
	}
	runContainersStatus(os.Args[2:])
}

func containerDBPath(dbFlag string) string {
	if dbFlag != "" {
		return dbFlag
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	return filepath.Join(root, ".herd", "container-lifecycle.db")
}

func runContainersStatus(args []string) {
	fs := flag.NewFlagSet("containers", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output JSON report")
	dbFlag := fs.String("db", "", "Path to the container lifecycle receipt DB (default $HERD_ROOT/.herd/container-lifecycle.db)")
	fs.Usage = func() {
		fmt.Println(`herd-containers , durable container lifecycle status (FAC-200):
  owned-active / owned-awaiting-cleanup / removed / quarantined receipts
  for every hermetic/containerized verification launch, plus a read-only
  audit of live docker containers with no receipt at all (pre-existing
  or created outside this store). Never removes anything itself — audit
  findings need independent, supervised cleanup (see
  docs/fac-200-fac174-reconciliation-plan.md for the FAC-174 baseline).

    herd containers                    # human summary
    herd containers --json             # machine-readable report
    herd containers reconcile          # dry-run: what a sweep would reclaim
    herd containers reconcile --apply  # actually reclaim dead-generation receipts`)
	}

	for _, a := range args {
		if a == "--help" || a == "-h" {
			fs.Usage()
			os.Exit(0)
		}
	}

	if err := fs.Parse(args); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-containers: %v\n", err)
		}
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "herd-containers: unknown arg %s\n", fs.Arg(0))
		os.Exit(2)
	}

	store, err := containerlifecycle.NewStore(containerDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-containers: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	report, err := containerlifecycle.Status(context.Background(), store, containerListerFunc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-containers: %v\n", err)
		os.Exit(1)
	}

	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "herd-containers: encode report: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}

	fmt.Printf("owned active:               %d\n", len(report.OwnedActive))
	fmt.Printf("owned awaiting cleanup:     %d\n", len(report.OwnedAwaitingCleanup))
	fmt.Printf("removed (absence proved):   %d\n", len(report.Removed))
	fmt.Printf("quarantined:                %d\n", len(report.Quarantined))
	if report.AuditError != "" {
		fmt.Fprintf(os.Stderr, "herd-containers: live audit unavailable: %s\n", report.AuditError)
		return
	}
	baseline, other := containerlifecycle.LabelFAC174Baseline(report.Unowned)
	fmt.Printf("unowned (live, no receipt): %d\n", len(report.Unowned))
	if len(baseline) > 0 {
		fmt.Printf("  of which FAC-174 baseline (see docs/fac-200-fac174-reconciliation-plan.md): %d\n", len(baseline))
	}
	if len(other) > 0 {
		fmt.Println("\nUNOWNED (not FAC-174 baseline) — independently audit ownership before removing:")
		for _, c := range other {
			fmt.Printf("  %s  %s  %s  %s\n", c.ID, c.Image, c.Status, c.Names)
		}
	}
}

func runContainersReconcile(args []string) {
	fs := flag.NewFlagSet("containers reconcile", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output JSON report")
	dbFlag := fs.String("db", "", "Path to the container lifecycle receipt DB (default $HERD_ROOT/.herd/container-lifecycle.db)")
	staleAfter := fs.Duration("stale-after", 30*time.Minute, "A receipt's generation is presumed dead once no receipt for it has been touched in this long")
	apply := fs.Bool("apply", false, "Actually remove reclaimable containers (default is a dry run report)")
	fs.Usage = func() {
		fmt.Println(`herd-containers reconcile , coordinator recovery sweep (FAC-200):
  reclaims containers whose receipts belong to a generation that has
  gone stale (no receipt update in --stale-after), the same recovery an
  agent interrupted between create and start needs. Only ever acts on
  containers this store already has a receipt for. Default is a dry
  run; pass --apply to actually remove.

    herd containers reconcile             # dry run
    herd containers reconcile --apply     # reclaim stale-generation containers`)
	}
	for _, a := range args {
		if a == "--help" || a == "-h" {
			fs.Usage()
			os.Exit(0)
		}
	}
	if err := fs.Parse(args); err != nil {
		if err != flag.ErrHelp {
			fmt.Fprintf(os.Stderr, "herd-containers reconcile: %v\n", err)
		}
		os.Exit(2)
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "herd-containers reconcile: unknown arg %s\n", fs.Arg(0))
		os.Exit(2)
	}

	store, err := containerlifecycle.NewStore(containerDBPath(*dbFlag))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-containers reconcile: %v\n", err)
		os.Exit(1)
	}
	defer store.Close()

	live := containerlifecycle.StaleGenerationLive(store, *staleAfter, time.Now)
	ctx := context.Background()

	if !*apply {
		candidates, err := store.ListNonTerminal()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd-containers reconcile: %v\n", err)
			os.Exit(1)
		}
		report := containerlifecycle.ReconcileReport{DryRun: true}
		for _, r := range candidates {
			if live(r.TaskRef, r.Generation) {
				report.Skipped = append(report.Skipped, r.ContainerID)
			} else {
				report.Reclaimed = append(report.Reclaimed, r.ContainerID)
			}
		}
		sort.Strings(report.Reclaimed)
		sort.Strings(report.Skipped)
		if *jsonFlag {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			if err := enc.Encode(report); err != nil {
				fmt.Fprintf(os.Stderr, "herd-containers reconcile: encode report: %v\n", err)
				os.Exit(1)
			}
			os.Exit(0)
		}
		fmt.Printf("dry run: would reclaim %d, still live %d (pass --apply to actually reclaim)\n", len(report.Reclaimed), len(report.Skipped))
		for _, id := range report.Reclaimed {
			fmt.Printf("  would reclaim: %s\n", id)
		}
		os.Exit(0)
	}

	report, err := containerlifecycle.Reconcile(ctx, store, live, containerlifecycle.DockerRemove, containerlifecycle.DockerAbsent)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-containers reconcile: %v\n", err)
		os.Exit(1)
	}

	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(os.Stderr, "herd-containers reconcile: encode report: %v\n", err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	fmt.Printf("reclaimed: %d, quarantined: %d, skipped (live): %d\n", len(report.Reclaimed), len(report.Quarantined), len(report.Skipped))
	for _, id := range report.Quarantined {
		fmt.Printf("  quarantined (needs review): %s\n", id)
	}
}

type verifyReaperReport struct {
	Present  bool   `json:"present"`
	Applied  bool   `json:"applied"`
	Output   string `json:"output,omitempty"`
	ExitCode int    `json:"exit_code,omitempty"`
	Error    string `json:"error,omitempty"`
}

// runRepoVerifyReaper delegates stack ownership, liveness, and selection to
// the repository that created the verify harness. The absent-reaper case is
// deliberately successful: a repository without a verify harness still gets
// its normal tab/worktree cleanup.
func runRepoVerifyReaper(ctx context.Context, repository string, apply bool) (verifyReaperReport, error) {
	const relativeReaper = "./bin/verify-stack-reap"
	path := filepath.Join(repository, "bin", "verify-stack-reap")
	info, err := os.Stat(path)
	if os.IsNotExist(err) || (err == nil && (info.IsDir() || info.Mode()&0o111 == 0)) {
		return verifyReaperReport{Output: "no repo reaper"}, nil
	}
	if err != nil {
		return verifyReaperReport{}, fmt.Errorf("inspect repo reaper: %w", err)
	}

	args := []string{}
	if apply {
		args = append(args, "--apply")
	}
	cmd := exec.CommandContext(ctx, relativeReaper, args...)
	cmd.Dir = repository
	out, runErr := cmd.CombinedOutput()
	report := verifyReaperReport{Present: true, Applied: apply, Output: strings.TrimSpace(string(out))}
	if runErr == nil {
		return report, nil
	}
	report.Error = runErr.Error()
	if exitErr, ok := runErr.(*exec.ExitError); ok {
		report.ExitCode = exitErr.ExitCode()
	}
	return report, fmt.Errorf("repo reaper failed: %w", runErr)
}
