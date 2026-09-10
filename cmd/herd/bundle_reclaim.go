package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/transfer"
)

type bundleReclaimOptions struct {
	transfer.ReclaimOptions
	jsonOut bool
}

func runBundleReclaim() {
	if err := runBundleReclaimArgs(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runBundleReclaimArgs(args []string) error {
	opts, err := parseBundleReclaimArgs(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	report, err := transfer.Reclaim(ctx, opts.ReclaimOptions)
	if err != nil {
		return err
	}
	if opts.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			return err
		}
		return nil
	}
	mode := "DRY_RUN"
	if opts.Act {
		mode = "ACT"
	}
	fmt.Printf("herd bundle-reclaim: %s root=%s scanned=%d candidates=%d reclaimed=%d reclaimed_bytes=%d would_reclaim_bytes=%d retained=%d partial=%t reason=%s\n",
		mode, opts.Root, report.Scanned, report.Candidates, report.Reclaimed, report.ReclaimedBytes, report.WouldReclaimBytes,
		len(report.Dispositions)-report.Reclaimed, report.Partial, report.Reason)
	for _, disp := range report.Dispositions {
		line := fmt.Sprintf("  %-52s %10d %s", disp.Name, disp.Bytes, disp.Action)
		if disp.Reason != "" {
			line += " " + disp.Reason
		}
		fmt.Println(line)
	}
	if !opts.Act && report.WouldReclaimBytes > 0 {
		fmt.Printf("herd bundle-reclaim: dry run only; no bytes were freed and nothing was deleted\n")
	}
	return nil
}

func parseBundleReclaimArgs(args []string) (bundleReclaimOptions, error) {
	var opts bundleReclaimOptions
	actSeen, dryRunSeen := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--act":
			if dryRunSeen {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: --act and --dry-run are contradictory")
			}
			actSeen = true
			opts.Act = true
		case "--dry-run":
			if actSeen {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: --act and --dry-run are contradictory")
			}
			dryRunSeen = true
			opts.Act = false
		case "--json":
			opts.jsonOut = true
		case "--root":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: %s requires a value", arg)
			}
			i++
			opts.Root = args[i]
		case "--max-files":
			n, err := parseBundleReclaimInt(arg, args, &i)
			if err != nil {
				return bundleReclaimOptions{}, err
			}
			opts.MaxFiles = n
		case "--max-bytes":
			n, err := parseBundleReclaimInt(arg, args, &i)
			if err != nil {
				return bundleReclaimOptions{}, err
			}
			opts.MaxBytes = int64(n)
		case "--min-age":
			if i+1 >= len(args) {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: %s requires a duration", arg)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: --min-age requires a non-negative duration")
			}
			opts.MinAge = d
		case "--protect":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: %s requires a comma-separated name list", arg)
			}
			i++
			for _, name := range strings.Split(args[i], ",") {
				if strings.TrimSpace(name) != "" {
					opts.Protect = append(opts.Protect, strings.TrimSpace(name))
				}
			}
		case "--manifest":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: %s requires a value", arg)
			}
			i++
			opts.Manifest = args[i]
		case "--lock-dir":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: %s requires a value", arg)
			}
			i++
			opts.LockDir = args[i]
		case "--lock-wait":
			if i+1 >= len(args) {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: %s requires a duration", arg)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: --lock-wait requires a non-negative duration")
			}
			opts.LockWait = d
		default:
			return bundleReclaimOptions{}, fmt.Errorf("herd bundle-reclaim: unknown flag %s", arg)
		}
	}
	if opts.Root == "" {
		return bundleReclaimOptions{}, fmt.Errorf("usage: herd bundle-reclaim --root <owned .herd bundle directory> --manifest <retention manifest> [--dry-run|--act] [--json] [--lock-dir DIR] [--lock-wait DUR] [--max-files N] [--max-bytes N] [--min-age DUR] [--protect NAME,NAME]")
	}
	opts.RepoRoot = lockCanonicalRoot()
	return opts, nil
}

func parseBundleReclaimInt(arg string, args []string, i *int) (int, error) {
	if *i+1 >= len(args) {
		return 0, fmt.Errorf("herd bundle-reclaim: %s requires a value", arg)
	}
	*i++
	n, err := strconv.Atoi(args[*i])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("herd bundle-reclaim: %s requires a positive integer", arg)
	}
	return n, nil
}
