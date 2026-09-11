package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/transfer"
)

// runBundleManifest produces the version-2 content-bound retention manifest
// that herd bundle-reclaim consumes, for EXPLICIT exact bundle names in an
// owned .herd root. Default is a dry run describing the intended manifest;
// only --write creates it, atomically and without overwriting.
func runBundleManifest() {
	if err := runBundleManifestArgs(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runBundleManifestArgs(args []string) error {
	opts, err := parseBundleManifestArgs(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	report, err := transfer.ProduceRetentionManifest(ctx, opts)
	if err != nil {
		return err
	}
	if report.Written {
		return nil
	}
	// Dry run: the manifest document itself was described on stderr; the
	// structured summary goes to stdout for scripts.
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func parseBundleManifestArgs(args []string) (transfer.ProduceOptions, error) {
	var opts transfer.ProduceOptions
	writeSeen, dryRunSeen := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--write":
			if dryRunSeen {
				return opts, fmt.Errorf("herd bundle-manifest: --write and --dry-run are contradictory")
			}
			writeSeen = true
			opts.Write = true
		case "--dry-run":
			if writeSeen {
				return opts, fmt.Errorf("herd bundle-manifest: --write and --dry-run are contradictory")
			}
			dryRunSeen = true
			opts.Write = false
		case "--bundle":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("herd bundle-manifest: %s requires an exact .bundle filename", arg)
			}
			i++
			opts.Bundles = append(opts.Bundles, args[i])
		case "--authority":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("herd bundle-manifest: %s requires a declared authority string", arg)
			}
			i++
			opts.Authority = args[i]
		case "--out":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("herd bundle-manifest: %s requires a value", arg)
			}
			i++
			opts.Out = args[i]
		case "--root":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("herd bundle-manifest: %s requires a value", arg)
			}
			i++
			opts.Root = args[i]
		case "--lock-dir":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return opts, fmt.Errorf("herd bundle-manifest: %s requires a value", arg)
			}
			i++
			opts.LockDir = args[i]
		case "--lock-wait":
			if i+1 >= len(args) {
				return opts, fmt.Errorf("herd bundle-manifest: %s requires a duration", arg)
			}
			i++
			d, err := time.ParseDuration(args[i])
			if err != nil || d < 0 {
				return opts, fmt.Errorf("herd bundle-manifest: --lock-wait requires a non-negative duration")
			}
			opts.LockWait = d
		case "-h", "--help":
			return opts, fmt.Errorf(`usage: herd bundle-manifest --root <owned .herd bundle directory> --bundle NAME [--bundle NAME...] --authority <declared authority> [--out <manifest path>] [--write|--dry-run] [--lock-dir DIR] [--lock-wait DUR]

Produces the version-2 content-bound retention manifest consumed by
` + "`herd bundle-reclaim`" + `. Schema (exactly the reclaim consumer's contract):

  {
    "version": 2,
    "authority": "<the explicit operator declaration, recorded verbatim>",
    "bundles": [
      {
        "name": "<exact .bundle filename>",
        "digest": "<64-hex sha256 streaming content digest>",
        "size": <positive integer bytes>,
        "mod_time_unix_nano": <positive integer nanoseconds>
      }
    ]
  }

Bundles are explicit names only — never enumerated, globbed, or auto-listed.
Each named bundle must be a regular non-symlink, non-hard-linked file whose
git bundle verifies with tips retained by canonical refs; identity is
captured under the native shared-checkout lock with integer nanosecond
modification identity. Any refusal fails the whole pass (no partial
manifest). --write creates the manifest atomically inside the owned root
with O_EXCL — an existing artifact is never overwritten. No bundle is ever
deleted, moved, or modified by this command.`)
		default:
			return opts, fmt.Errorf("herd bundle-manifest: unknown flag %s", arg)
		}
	}
	if opts.Root == "" || len(opts.Bundles) == 0 || strings.TrimSpace(opts.Authority) == "" {
		return opts, fmt.Errorf("usage: herd bundle-manifest --root <owned .herd bundle directory> --bundle NAME [--bundle NAME...] --authority <declared authority> [--out <manifest path>] [--write|--dry-run] [--lock-dir DIR] [--lock-wait DUR]")
	}
	if opts.Write && strings.TrimSpace(opts.Out) == "" {
		return opts, fmt.Errorf("herd bundle-manifest: --write requires --out")
	}
	opts.RepoRoot = lockCanonicalRoot()
	return opts, nil
}
