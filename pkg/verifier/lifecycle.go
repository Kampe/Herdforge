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
// the group id (leader pid when Setpgid:true). Production Cancel and
// ReapOwnedCmd use this; tests may replace it to mutation-prove that
// group-wide kill (not leader-only) is required.
var processGroupKiller = killProcessGroup

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

// processGroupGoneBound is how long ReapOwnedCmd polls for the process group
// to empty after SIGKILL+Wait. Orphaned zombies can keep kill(-pgid,0)==nil
// until init reaps them; this bound is a fail-closed ownership gate, not a
// cancel/cleanup sleep.
// Bound covers zombie reaping after SIGKILL; live descendants fail closed at
// this bound (leader-only kill mutation). Kept short enough for stress matrices.
const processGroupGoneBound = 500 * time.Millisecond

// ReapOwnedCmd is the production ownership close for a started verification
// subprocess: full process-group SIGKILL, Wait on the leader, then verify the
// group has no remaining members (grandchildren included). Errors are returned
// and must not be ignored by callers.
//
// Cancel paths that run under cmd.Wait must only call KillProcessGroup (not
// this function) to avoid double-Wait; ReapOwnedCmd is for fail-safe close
// when the caller owns the Wait (e.g. ctx already done after Start).
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
	// Group-wide liveness: any remaining member (including grandchildren) keeps
	// kill(-pgid, 0) succeeding. Leader-only kill fails this check. Poll until
	// the group is empty so transient zombies after SIGKILL do not false-fail
	// a correct group kill; a surviving live member fails closed at the bound.
	deadline := time.Now().Add(processGroupGoneBound)
	var lastProbe error
	for {
		lastProbe = syscall.Kill(-pgid, 0)
		if lastProbe != nil && isESRCH(lastProbe) {
			break
		}
		if time.Now().After(deadline) {
			_ = processGroupKiller(pgid)
			if lastProbe == nil {
				return fmt.Errorf("reap owned cmd: process group %d still has live members after kill+wait", pgid)
			}
			return fmt.Errorf("reap owned cmd: probe process group %d: %w", pgid, lastProbe)
		}
		// Re-signal in case a late fork raced the first group kill.
		_ = processGroupKiller(pgid)
		time.Sleep(time.Millisecond)
	}
	if waitErr != nil && !isExpectedKillWait(waitErr) {
		return fmt.Errorf("reap owned cmd: wait after group kill: %w", waitErr)
	}
	return nil
}

// reapProcessGroup is a best-effort kill used only where Wait is owned
// elsewhere (CommandContext Cancel). Prefer ReapOwnedCmd when the caller
// owns Wait. Errors are discarded only at this thin adapter; production
// Cancel surfaces KillProcessGroup errors via the Cancel return value.
func reapProcessGroup(pgid int) {
	_ = processGroupKiller(pgid)
}

func isESRCH(err error) bool {
	return err != nil && (errors.Is(err, syscall.ESRCH) || err == syscall.ESRCH)
}

func isExpectedKillWait(err error) bool {
	if err == nil {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "signal") || strings.Contains(msg, "kill") || strings.Contains(msg, "killed")
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
