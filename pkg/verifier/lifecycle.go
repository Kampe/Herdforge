package verifier

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// lifecycle owns subprocess process-groups for one explicit session (tests or a
// single mutation transaction). Production helpers reap their own process group
// after Wait without sharing mutable session state across goroutines.
//
// Detached git auto-gc / maintenance / fsmonitor writers are prevented at the
// source by hermeticGitCommand; residual group members from canceled shells
// are reaped explicitly. No sleeps or RemoveAll retries.
type lifecycle struct {
	mu    sync.Mutex
	pgids []int
}

func (l *lifecycle) track(pgid int) {
	if l == nil || pgid <= 0 {
		return
	}
	l.mu.Lock()
	l.pgids = append(l.pgids, pgid)
	l.mu.Unlock()
}

// reap kills and forgets every tracked process group. Safe to call multiple
// times. Used by tests that need an explicit barrier before TempDir cleanup.
func (l *lifecycle) reap() {
	if l == nil {
		return
	}
	l.mu.Lock()
	pgids := append([]int(nil), l.pgids...)
	l.pgids = l.pgids[:0]
	l.mu.Unlock()
	for _, pgid := range pgids {
		reapProcessGroup(pgid)
	}
}

// reapProcessGroup SIGKILLs the entire process group. After cmd.Wait the
// leader is already reaped; residual grandchildren (shells that double-forked
// without setsid) may still hold directories open. Immediate kill of the group
// is the deterministic barrier — not a delayed retry.
func reapProcessGroup(pgid int) {
	if pgid <= 0 {
		return
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
}

// observeStarted records the process group of a started command whose
// SysProcAttr.Setpgid is true (leader pid == pgid).
func (l *lifecycle) observeStarted(cmd *exec.Cmd) {
	if l == nil || cmd == nil || cmd.Process == nil {
		return
	}
	l.track(cmd.Process.Pid)
}

// finishCommand reaps residual group members after Wait and drops the pgid
// from the session.
func (l *lifecycle) finishCommand(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	reapProcessGroup(pgid)
	if l == nil {
		return
	}
	l.mu.Lock()
	out := l.pgids[:0]
	for _, id := range l.pgids {
		if id != pgid {
			out = append(out, id)
		}
	}
	l.pgids = out
	l.mu.Unlock()
}

// hermeticGitConfig is prepended to every git argv. These flags stop git from
// spawning detached auto-gc / maintenance / fsmonitor writers into the
// candidate's .git — the failure class observed as:
//
//	TempDir RemoveAll cleanup: unlinkat .../.git: directory not empty
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
//   - runs in its own process group for explicit reap
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

// runGit runs hermetic git, waits for the leader, then reaps residual process
// group members. Concurrent callers do not share session state.
func runGit(dir string, args ...string) ([]byte, error) {
	cmd := hermeticGitCommand(dir, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	waitErr := cmd.Wait()
	if cmd.Process != nil {
		reapProcessGroup(cmd.Process.Pid)
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return stdout.Bytes(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.Bytes(), nil
}

// diagnoseRepoWriters lists relative basenames still under root/.git for
// cleanup diagnostics. Paths are rooted at root so host absolute prefixes never
// leave the diagnostic string.
func diagnoseRepoWriters(root string) string {
	gitDir := filepath.Join(root, ".git")
	var found []string
	_ = filepath.Walk(gitDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = filepath.Base(path)
		}
		found = append(found, rel)
		if len(found) >= 16 {
			return filepath.SkipAll
		}
		return nil
	})
	if len(found) == 0 {
		return "no residual files under .git"
	}
	return "residual under .git: " + strings.Join(found, ", ")
}
