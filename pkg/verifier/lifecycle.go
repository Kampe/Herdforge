package verifier

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// processGroupKiller SIGKILLs every member of a process group. The argument is
// the group id (leader pid when Setpgid:true). Production Cancel,
// ReapOwnedCmd, and closeOwnedAfterWait use this; tests may replace it to
// mutation-prove that group-wide kill (not leader-only) is required.
var processGroupKiller = killProcessGroup

// afterWaitOwnership is the production post-Wait ownership close for execute.
// Tests may replace it to mutation-prove that omitting residual process-group
// reap lets execute return while background/detached same-group writers live.
var afterWaitOwnership = closeOwnedAfterWait

func killProcessGroup(pgid int) error {
	if pgid <= 0 {
		return fmt.Errorf("kill process group: invalid pgid %d", pgid)
	}
	err := syscall.Kill(-pgid, syscall.SIGKILL)
	if err == nil || isESRCH(err) {
		return nil
	}
	return err
}

// KillProcessGroup is the production group-wide SIGKILL primitive. It returns
// nil when the group is already gone (ESRCH).
func KillProcessGroup(pgid int) error {
	return processGroupKiller(pgid)
}

// processGroupGoneBound is how long ownership close polls for the process group
// to empty after SIGKILL. Orphaned zombies can keep kill(-pgid,0)==nil until
// init reaps them; this bound is a fail-closed ownership gate, not a
// cancel/cleanup sleep. Live descendants fail closed at this bound.
const processGroupGoneBound = 500 * time.Millisecond

// ErrResidualProcessGroup is returned when the leader has been Waited but the
// process group still had live members (background jobs / same-group writers).
// execute maps this to BLOCKED so TempDir cleanup cannot race residual writers.
var ErrResidualProcessGroup = errors.New("residual process group members after leader wait")

// waitProcessGroupEmpty polls until no member of pgid remains. Re-signals the
// group while waiting so late forks cannot slip past a single kill.
func waitProcessGroupEmpty(pgid int) error {
	deadline := time.Now().Add(processGroupGoneBound)
	var lastProbe error
	for {
		lastProbe = syscall.Kill(-pgid, 0)
		if lastProbe != nil && isESRCH(lastProbe) {
			return nil
		}
		if time.Now().After(deadline) {
			_ = processGroupKiller(pgid)
			if lastProbe == nil {
				return fmt.Errorf("process group %d still has live members", pgid)
			}
			return fmt.Errorf("probe process group %d: %w", pgid, lastProbe)
		}
		_ = processGroupKiller(pgid)
		time.Sleep(time.Millisecond)
	}
}

// processGroupLive reports whether any member of pgid is still known to the kernel.
func processGroupLive(pgid int) bool {
	err := syscall.Kill(-pgid, 0)
	return err == nil
}

// ReapOwnedCmd is the production ownership close when the caller still owns
// Wait: full process-group SIGKILL, Wait on the leader, then prove the group
// has no remaining members (grandchildren included). Errors are returned and
// must not be ignored.
//
// Cancel paths that run under cmd.Wait must only call KillProcessGroup (not
// this function) to avoid double-Wait; use closeOwnedAfterWait after Wait.
// ReapOwnedCmd is for fail-safe close when ctx is already done after Start.
func ReapOwnedCmd(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("reap owned cmd: nil process")
	}
	pgid := cmd.Process.Pid
	if err := processGroupKiller(pgid); err != nil {
		return fmt.Errorf("reap owned cmd: kill process group %d: %w", pgid, err)
	}
	waitErr := cmd.Wait()
	if cmd.ProcessState == nil {
		return fmt.Errorf("reap owned cmd: wait returned without ProcessState: %v", waitErr)
	}
	if err := waitProcessGroupEmpty(pgid); err != nil {
		return fmt.Errorf("reap owned cmd: %w", err)
	}
	if waitErr != nil && !isExpectedKillWait(waitErr) {
		return fmt.Errorf("reap owned cmd: wait after group kill: %w", waitErr)
	}
	return nil
}

// closeOwnedAfterWait is the production ownership close after cmd.Wait has
// already reaped the leader (and joined stdout/stderr copy goroutines).
// It SIGKILLs any residual process-group members (background/same-group
// writers that outlived the leader), proves the group is empty, and returns
// ErrResidualProcessGroup when members were still live after Wait — execute
// must not return PASS while owned descendants remain.
//
// Does not call Wait (no double-Wait). Safe after Cancel+Wait and after a
// normal success/fail Wait.
func closeOwnedAfterWait(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return errors.New("close owned after wait: nil process")
	}
	if cmd.ProcessState == nil {
		return errors.New("close owned after wait: ProcessState nil (Wait not completed)")
	}
	pgid := cmd.Process.Pid
	residual := processGroupLive(pgid)
	if err := processGroupKiller(pgid); err != nil {
		return fmt.Errorf("close owned after wait: kill process group %d: %w", pgid, err)
	}
	if err := waitProcessGroupEmpty(pgid); err != nil {
		return fmt.Errorf("close owned after wait: %w", err)
	}
	if residual {
		return fmt.Errorf("%w: pgid %d", ErrResidualProcessGroup, pgid)
	}
	return nil
}

// reapProcessGroup is a best-effort kill used only where Wait is owned
// elsewhere (CommandContext Cancel). Prefer ReapOwnedCmd / closeOwnedAfterWait
// when the caller owns the full transaction. Production Cancel surfaces
// KillProcessGroup errors via the Cancel return value.
func reapProcessGroup(pgid int) {
	_ = processGroupKiller(pgid)
}

func isESRCH(err error) bool {
	return err != nil && (errors.Is(err, syscall.ESRCH) || err == syscall.ESRCH)
}

// isExpectedKillWait reports whether err is an exit caused by SIGKILL/SIGTERM
// (or already nil). Uses typed syscall.WaitStatus — not substring matching.
func isExpectedKillWait(err error) bool {
	if err == nil {
		return true
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	status, ok := ee.Sys().(syscall.WaitStatus)
	if !ok {
		return false
	}
	if !status.Signaled() {
		return false
	}
	sig := status.Signal()
	return sig == syscall.SIGKILL || sig == syscall.SIGTERM
}

// waitStatusSignaled reports whether ProcessState was terminated by signal.
func waitStatusSignaled(state *os.ProcessState) (syscall.Signal, bool) {
	if state == nil {
		return 0, false
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return 0, false
	}
	return status.Signal(), true
}

// hermeticGitConfig is prepended to every git argv. These flags stop git from
// spawning detached auto-gc / maintenance / fsmonitor writers into the
// candidate's .git — the failure class observed as:
//
//	TempDir RemoveAll cleanup: unlinkat .../.git: directory not empty
//
// Coverage: TestHermeticGitConfigFlagsReachGit asserts git resolves these
// values via `git config --get` under the same -c prefix (deleting this
// slice fails that test).
var hermeticGitConfig = []string{
	"-c", "gc.auto=0",
	"-c", "gc.autoDetach=false",
	"-c", "maintenance.auto=false",
	"-c", "core.fsmonitor=",
	"-c", "core.useBuiltinFSMonitor=false",
	// Point hooks at a non-directory so ambient core.hooksPath cannot run.
	// /dev/null is portable on the CI and macOS hosts this package targets.
	"-c", "core.hooksPath=/dev/null",
}

func hermeticGitEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env)+4)
	for _, entry := range env {
		switch {
		case strings.HasPrefix(entry, "GIT_CONFIG_GLOBAL="),
			strings.HasPrefix(entry, "GIT_CONFIG_SYSTEM="),
			strings.HasPrefix(entry, "GIT_CONFIG_NOSYSTEM="),
			strings.HasPrefix(entry, "GIT_CONFIG_COUNT="),
			strings.HasPrefix(entry, "GIT_CONFIG_PARAMETERS="),
			strings.HasPrefix(entry, "GIT_TRACE"):
			continue
		default:
			out = append(out, entry)
		}
	}
	out = append(out,
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
	)
	return out
}

// hermeticGitCommand builds a git subprocess that:
//   - cannot inherit ambient global/system config writers
//   - cannot auto-detach gc/maintenance/fsmonitor into dir/.git
//   - runs in its own process group so Cancel/ReapOwnedCmd can reap the group
func hermeticGitCommand(dir string, args ...string) *exec.Cmd {
	full := make([]string, 0, len(hermeticGitConfig)+len(args))
	full = append(full, hermeticGitConfig...)
	full = append(full, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = hermeticGitEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd
}

// runGit runs hermetic git and waits for the leader. Detached git writers are
// prevented by hermeticGitConfig (asserted by TestHermeticGitConfigFlagsReachGit).
func runGit(dir string, args ...string) ([]byte, error) {
	cmd := hermeticGitCommand(dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}
