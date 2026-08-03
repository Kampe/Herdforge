package verifier

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// processGroupKiller SIGKILLs every member of a process group. The argument is
// the group id (leader pid when Setpgid:true). Production Cancel uses this
// while the leader is live; tests may replace it to mutation-prove that
// group-wide reap (not leader-only kill) is required.
var processGroupKiller = killProcessGroup

func killProcessGroup(pgid int) error {
	if pgid <= 0 {
		return nil
	}
	return syscall.Kill(-pgid, syscall.SIGKILL)
}

// reapProcessGroup SIGKILLs the entire process group via processGroupKiller.
func reapProcessGroup(pgid int) {
	_ = processGroupKiller(pgid)
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
//   - runs in its own process group so Cancel can reap the group
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
