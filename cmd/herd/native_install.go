package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/harvest"
)

type nativeInstallOptions struct {
	source   string
	revision string
	target   string
	act      bool
}

type nativeInstallBuild func(context.Context, string) error

func runNativeInstall(args []string) error {
	opts, err := parseNativeInstallArgs(args)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	binding, err := executeNativeInstall(ctx, opts, buildNativeRuntime)
	if err != nil {
		return err
	}
	if binding == nil {
		fmt.Printf("herd install: DRY_RUN source=%s revision=%s target=%s\n", opts.source, opts.revision, opts.target)
		return nil
	}
	fmt.Printf("herd install: installed revision=%s digest=%s target=%s\n", binding.Revision, binding.Digest, opts.target)
	return nil
}

func parseNativeInstallArgs(args []string) (nativeInstallOptions, error) {
	var opts nativeInstallOptions
	actSeen, dryRunSeen := false, false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--act":
			if dryRunSeen {
				return nativeInstallOptions{}, fmt.Errorf("herd install: --act and --dry-run are contradictory")
			}
			actSeen = true
			opts.act = true
		case "--dry-run":
			if actSeen {
				return nativeInstallOptions{}, fmt.Errorf("herd install: --act and --dry-run are contradictory")
			}
			dryRunSeen = true
			opts.act = false
		case "--source", "--revision", "--target":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "-") {
				return nativeInstallOptions{}, fmt.Errorf("herd install: %s requires a value", arg)
			}
			i++
			switch arg {
			case "--source":
				opts.source = args[i]
			case "--revision":
				opts.revision = args[i]
			case "--target":
				opts.target = args[i]
			}
		default:
			return nativeInstallOptions{}, fmt.Errorf("herd install: unknown flag %s", arg)
		}
	}
	if opts.source == "" || opts.revision == "" || opts.target == "" {
		return nativeInstallOptions{}, fmt.Errorf("usage: herd install --source <worktree> --revision <full-sha> --target <canonical-root> [--act|--dry-run]")
	}
	if len(opts.revision) != 40 {
		return nativeInstallOptions{}, fmt.Errorf("herd install: --revision must be a full 40-character SHA")
	}
	return opts, nil
}

func executeNativeInstall(ctx context.Context, opts nativeInstallOptions, build nativeInstallBuild) (*harvest.RuntimeBinding, error) {
	if !harvest.RuntimeInstallSupported() {
		return nil, fmt.Errorf("herd install: unsupported platform: runtime install capability is unavailable, refusing before build or mutation")
	}
	source, err := filepath.Abs(opts.source)
	if err != nil {
		return nil, fmt.Errorf("herd install: source path: %w", err)
	}
	target, err := filepath.Abs(opts.target)
	if err != nil {
		return nil, fmt.Errorf("herd install: target path: %w", err)
	}
	installer := harvest.HerdRuntimeInstaller{Root: target, Source: source, Revision: opts.revision}
	// This read-only check is deliberately before the build. It rejects a
	// foreign/changed canonical target before the source worktree is touched.
	current, err := installer.ObserveInstallation(ctx)
	if err != nil {
		return nil, err
	}
	if !opts.act {
		return nil, nil
	}
	if current != nil {
		// An exact-revision retry must resolve or keep surfacing a
		// previously returned hard retention-maintenance failure; it must
		// never silently succeed over retained unresolved state.
		return installer.ResolveRetainedMaintenance(ctx, current)
	}
	if err := build(ctx, source); err != nil {
		return nil, fmt.Errorf("herd install: build refused: %w", err)
	}
	binding, err := installer.Install(ctx)
	if err != nil {
		return binding, err
	}
	return binding, nil
}

func buildNativeRuntime(ctx context.Context, source string) error {
	cmd := exec.CommandContext(ctx, "sh", "./scripts/build-herd.sh", ".")
	cmd.Dir = source
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// The build runs in its own process group so a timeout or cancellation
	// can terminate the shell AND every toolchain descendant; without this a
	// stuck child survives the deadline and keeps writing under the source
	// worktree.
	if err := startProcessGroup(cmd); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		// Kill the whole group (negative pid), then reap the direct child so
		// no zombie or descendant outlives the deadline cleanup claim.
		_ = killProcessGroup(cmd.Process.Pid)
		waitErr := <-done
		if err := ctx.Err(); err != nil && waitErr == nil {
			return err
		}
		return ctx.Err()
	}
}
