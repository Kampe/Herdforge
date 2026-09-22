package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

func runArtifactArchive() {
	if err := runArtifactArchiveArgs(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runArtifactArchiveArgs(args []string) error {
	opts, err := parseArtifactArchiveArgs(args)
	if err != nil {
		return err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("artifact-archive: cwd: %w", err)
	}
	root, err := execGitRoot(cwd)
	if err != nil {
		return err
	}
	req := worktree.ArchiveRequest{
		Root:    root,
		Target:  opts.target,
		Archive: opts.archive,
		Act:     opts.act,
	}
	if opts.plan != "" && opts.act {
		raw, err := os.ReadFile(opts.plan)
		if err != nil {
			return fmt.Errorf("artifact-archive: read plan: %w", err)
		}
		var plan worktree.ArchivePlan
		if err := json.Unmarshal(raw, &plan); err != nil {
			return fmt.Errorf("artifact-archive: parse plan: %w", err)
		}
		req.Plan = &plan
	}
	rep, err := worktree.ArchiveIgnored(req)
	if err != nil {
		return err
	}
	if opts.plan != "" && !opts.act && rep.Plan != nil {
		body, err := json.MarshalIndent(rep.Plan, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(opts.plan, body, 0o600); err != nil {
			return fmt.Errorf("artifact-archive: write plan: %w", err)
		}
	}
	if opts.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	mode := "DRY_RUN"
	if opts.act {
		mode = "ACT"
	}
	fmt.Printf("herd artifact-archive: %s target=%s archived=%d removed=%d source_bytes=%d source_allocated_bytes=%d archive_new_bytes=%d dedup_savings=%d relocated_bytes=%d logical_unlinked_bytes=%d physical_reclaim_certain_bytes=%d physical_reclaim_uncertain_bytes=%d net_reclaim=%d\n",
		mode, rep.Target, rep.Archived, rep.Removed, rep.SourceBytes, rep.SourceAllocatedBytes, rep.ArchiveNewBytes, rep.DedupSavings, rep.RelocatedBytes, rep.LogicalUnlinkedBytes, rep.PhysicalReclaimCertainBytes, rep.PhysicalReclaimUncertainBytes, rep.NetReclaim)
	for _, e := range rep.Entries {
		fmt.Printf("  %-8s %-11s %-10s %s %s\n", e.Kind, e.Retention, e.Disposition, e.Digest[:12], e.Path)
	}
	if !opts.act {
		fmt.Println("herd artifact-archive: dry-run only; relocation is not reclaim")
	} else if rep.Manifest != "" {
		fmt.Printf("herd artifact-archive: manifest %s\n", rep.Manifest)
	}
	return nil
}

type artifactArchiveOpts struct {
	target  string
	archive string
	plan    string
	act     bool
	jsonOut bool
}

func parseArtifactArchiveArgs(args []string) (artifactArchiveOpts, error) {
	var opts artifactArchiveOpts
	act, dry := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--act":
			if dry {
				return opts, fmt.Errorf("artifact-archive: --act and --dry-run are contradictory")
			}
			act = true
			opts.act = true
		case "--dry-run":
			if act {
				return opts, fmt.Errorf("artifact-archive: --act and --dry-run are contradictory")
			}
			dry = true
		case "--json":
			opts.jsonOut = true
		case "--target":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("artifact-archive: --target requires a path")
			}
			i++
			opts.target = args[i]
		case "--archive":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("artifact-archive: --archive requires a path")
			}
			i++
			opts.archive = args[i]
		case "--plan":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("artifact-archive: --plan requires a path")
			}
			i++
			opts.plan = args[i]
		case "--help", "-h":
			return opts, fmt.Errorf("usage: herd artifact-archive --target <worktree> [--archive DIR] [--plan FILE] [--dry-run|--act] [--json]")
		default:
			return opts, fmt.Errorf("artifact-archive: unknown flag %s", arg)
		}
	}
	if strings.TrimSpace(opts.target) == "" {
		return opts, fmt.Errorf("usage: herd artifact-archive --target <worktree> [--archive DIR] [--plan FILE] [--dry-run|--act] [--json]")
	}
	return opts, nil
}

func execGitRoot(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("artifact-archive: canonical git root: %w", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		return "", fmt.Errorf("artifact-archive: canonical git root is empty")
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs, nil
	}
	return root, nil
}
