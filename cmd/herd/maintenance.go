package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/Kampe/Herdforge/pkg/daemon"
)

// herd maintenance is the unattended carrier for the FAC-805 cleanup beat.
//
// The beat itself already exists and is reviewed: runReapPulseTick. Its only
// callers are `herd pulse --act` and `herd daemon`, and both refuse to start
// without provider posture -- pulse runs requireFleetAdmission before any beat
// work, and the daemon gates on the same authority and then resolves a task
// provider and dispatches lanes. A repository with no admitted coordinator
// therefore never cleans up, which is the state this checkout was found in:
// the host's only pulse timer beats a different repository.
//
// This command supplies the missing schedule and nothing else. It adds no
// classification, no removal authority, and no lock of its own: it drives the
// same bounded tick under the same kernel tick lock, so a maintenance cycle
// and a pulse beat exclude each other rather than racing. It holds no
// admission authority, builds no task provider, and never reaches dispatch, so
// it cannot be refused by posture and cannot open a work lane.
func runMaintenance() {
	os.Exit(runMaintenanceCommand(os.Args[2:], os.Stdout, os.Stderr))
}

// maintenanceTick is the cleanup beat this carrier schedules. It is a variable
// so the cadence, disposition, and exit contract can be proven without a real
// retirement, and so the beat's own contract stays owned by its own file.
var maintenanceTick = runReapPulseTick

func runMaintenanceCommand(args []string, out, errOut io.Writer) int {
	// A supervisor stops this job with SIGTERM. Cancellation reaches the tick's
	// own checkpoints, which is a bounded stop, not a kill: a retirement batch
	// already begun is finished rather than torn in half.
	ctx, stopSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignals()
	return runMaintenanceCommandContext(ctx, args, out, errOut)
}

func runMaintenanceCommandContext(ctx context.Context, args []string, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("maintenance", flag.ContinueOnError)
	fs.SetOutput(errOut)
	act := fs.Bool("act", false, "retire the landed worktrees this cycle finds; without it, report only")
	interval := fs.Duration("interval", 0, "resident cadence between cycles; zero runs one cycle and exits")
	maxCycles := fs.Int("max-cycles", 0, "stop after this many cycles; zero runs until cancelled (requires --interval)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "maintenance: unexpected argument %q\n", fs.Arg(0))
		return 2
	}
	if *interval < 0 {
		fmt.Fprintln(errOut, "maintenance: --interval cannot be negative")
		return 2
	}
	if *maxCycles < 0 {
		fmt.Fprintln(errOut, "maintenance: --max-cycles cannot be negative")
		return 2
	}
	if *maxCycles > 0 && *interval == 0 {
		fmt.Fprintln(errOut, "maintenance: --max-cycles needs --interval; without a cadence there is exactly one cycle")
		return 2
	}

	// Locks, config, and the beat cursor all resolve against the canonical
	// repository, so a carrier started from a linked worktree shares state with
	// the pulse beat instead of opening a second one beside it. The base comes
	// from the repository's configured default branch, not a hardcoded ref.
	root := canonicalRepoRoot(firstEnv("HERD_ROOT", "HERD_REPO_ROOT", "."))
	base := reapPulseBaseRef(root)

	// Zero interval is one cycle and exit: the supervisor's own timer is the
	// cadence and no resident process is left behind. MaxTicks expresses both
	// modes on the one scheduler the daemon already uses.
	ticks := *maxCycles
	if *interval == 0 {
		ticks = 1
	}

	// The scheduler keeps ticking after a failed tick and reports only the
	// first error, so it cannot be the exit authority: faults are counted here
	// and decide the exit code directly. Both this count and the scheduler's
	// own result are judged against parent health rather than error identity.
	cycles, faults := 0, 0
	schedErr := daemon.RunPulseScheduler(ctx, daemon.PulseSchedulerOptions{Interval: *interval, MaxTicks: ticks}, func(tickCtx context.Context) error {
		cycles++
		report, err := maintenanceTick(tickCtx, root, base, *act)
		switch {
		case errors.Is(err, errReapPulseTickBusy):
			// A pulse beat or another carrier holds the tick lock and is doing
			// the work. Deferring is the exclusion working, not a fault.
			fmt.Fprintf(errOut, "maintenance: cycle %d deferred: %v\n", cycles, err)
			return nil
		case err != nil:
			// Parent health, not the error's identity, decides. A cycle that
			// fails while the parent context is still healthy is a cleanup
			// fault even when it reports Canceled or DeadlineExceeded: those
			// come from a child bound inside the tick, and treating them as an
			// orderly stop would exit 0 on cleanup that never happened. Only a
			// genuine shutdown of this process is a clean stop.
			if ctx.Err() != nil {
				return err
			}
			faults++
			fmt.Fprintf(errOut, "maintenance: cycle %d: %v\n", cycles, err)
			return err
		}
		fmt.Fprintf(out, "maintenance: cycle %d registered=%d eligible=%d inspected=%d landed=%d retired=%d failed=%d acted=%t\n",
			cycles, report.Registered, report.Eligible, report.Inspected,
			report.Landed, report.Retired, report.Failed, report.Acted)
		if report.Failed > 0 {
			faults++
		}
		return nil
	})

	switch {
	case faults > 0:
		fmt.Fprintf(errOut, "maintenance: %d of %d cycle(s) failed\n", faults, cycles)
		return 1
	case schedErr != nil && ctx.Err() == nil:
		fmt.Fprintf(errOut, "maintenance: %v\n", schedErr)
		return 1
	case schedErr != nil:
		fmt.Fprintf(errOut, "maintenance: stopped after %d cycle(s)\n", cycles)
		return 0
	default:
		return 0
	}
}
