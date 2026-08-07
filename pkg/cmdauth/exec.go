package cmdauth

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/procsignal"
)

// Spawn creates one process and returns its exit code. It is reached only
// after an attempt has been durably consumed.
type Spawn func(ctx context.Context, dir string, argv []string) (exitCode int, err error)

// Outcome is the result of a guarded execution.
type Outcome struct {
	Grant    Grant `json:"grant"`
	Ran      bool  `json:"ran"`
	ExitCode int   `json:"exit_code"`
}

// Run is THE execution boundary. It consumes exactly one attempt durably and
// atomically, and only then calls spawn. Order matters and is not negotiable:
// if the process were created first, a crash between exec and the ledger write
// would hand the budget back, which is the same hole prompt wording had.
//
// The presented hash is recomputed here from the exact dir/argv about to run
// rather than taken from req — an executor cannot present the hash of the
// authorized command while spawning a different one.
//
// On refusal nothing is spawned: Run returns before it builds an exec.Cmd, so
// a rejected attempt has no child process, no signal, and no write anywhere
// but this package's own append-only ledger.
func (s *Store) Run(ctx context.Context, req Request, dir string, argv []string, spawn Spawn) (Outcome, error) {
	if len(argv) == 0 {
		return Outcome{}, fmt.Errorf("cmdauth: refusing to run an empty command")
	}
	if spawn == nil {
		return Outcome{}, fmt.Errorf("cmdauth: no spawn provided")
	}
	req.CommandHash = CanonicalHash(dir, argv)

	grant, err := s.Consume(ctx, req)
	if err != nil {
		return Outcome{}, err
	}

	exitCode, runErr := spawn(ctx, dir, argv)
	if runErr != nil {
		// The attempt is already spent, and correctly so: a command that
		// could not start still burned its authorization. Record it as a
		// failure so a stop-on-first-failure token is burned too.
		exitCode = -1
	}
	if err := s.RecordOutcome(ctx, grant, exitCode); err != nil {
		return Outcome{Grant: grant, Ran: true, ExitCode: exitCode},
			fmt.Errorf("cmdauth: command ran but its outcome could not be recorded: %w", err)
	}
	out := Outcome{Grant: grant, Ran: true, ExitCode: exitCode}
	if runErr != nil {
		return out, fmt.Errorf("cmdauth: authorized command failed to start: %w", runErr)
	}
	return out, nil
}

// OwnedSpawn is the production Spawn: an owned process group torn down
// through procsignal, matching pkg/verifier's runShell contract (FAC-174 /
// FAC-192). Setpgid owns the tree; Cancel claims the live leader and tears
// the group down via the opaque entrypoint — never a raw kill(-pgid), never
// leader-only Kill that strands grandchildren.
func OwnedSpawn(stdout, stderr io.Writer) Spawn {
	return func(ctx context.Context, dir string, argv []string) (int, error) {
		if ctx == nil {
			// Fail closed rather than spawn an uncancellable child.
			return -1, errors.New("cmdauth: nil context")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = dir
		cmd.Stdout = stdout
		cmd.Stderr = stderr
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		cmd.Cancel = func() error {
			if cmd.Process == nil {
				return nil
			}
			return procsignal.CancelSpawnedProcess(cmd.Process)
		}
		// Bound the pipe drain after cancel so a grandchild holding stdout
		// cannot stall Wait indefinitely.
		cmd.WaitDelay = 100 * time.Millisecond
		err := cmd.Run()
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode(), nil
		}
		if err != nil {
			return -1, err
		}
		return 0, nil
	}
}
