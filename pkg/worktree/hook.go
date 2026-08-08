package worktree

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// preRebaseHookScript is the shell script installed as the pre-rebase hook
// in every task worktree. It auto-captures the lane's current HEAD to
// refs/herd/safe/<task> before any rebase begins — so even when the
// coordinator forgets to call WriteSafeRef, the tip is captured
// automatically.
//
// The hook is POSIX sh, uses only git and tr (both universally available),
// and ALWAYS exits 0: the safe ref is a safety net, never a gate. A failure
// to write the safe ref must never block a rebase.
//
// FAC-214: this is the "automatic" half of the safe-ref protection. Without
// it, the coordinator had to remember to call WriteSafeRef before every
// rebase instruction. With it, the hook fires every time `git rebase` runs
// in the worktree, capturing the pre-rebase tip while HEAD still points at
// the lane's work.
const preRebaseHookScript = `#!/bin/sh
# FAC-214 pre-rebase: auto-capture lane tip to refs/herd/safe/<task>.
# Runs before git rebase starts, while HEAD still points at the pre-rebase
# tip. If the lane runs ` + "`git reset --hard origin/main`" + ` after
# ` + "`git rebase --abort`" + `, the captured commits stay reachable.
# Always exits 0 — the safe ref is a safety net, never a gate.
branch=$(git symbolic-ref --short HEAD 2>/dev/null) || exit 0
case "$branch" in
herd/*)
  task=${branch#herd/}
  sha=$(git rev-parse --verify HEAD 2>/dev/null) || exit 0
  ref="refs/herd/safe/$(printf '%s' "$task" | tr 'A-Z' 'a-z')"
  git update-ref "$ref" "$sha" 2>/dev/null || true
  ;;
esac
exit 0
`

// InstallPreRebaseHook installs a pre-rebase hook in the worktree's git
// hooks directory. The hook auto-writes refs/herd/safe/<task> before any
// rebase, making the safe-ref capture automatic rather than dependent on
// the coordinator remembering to call WriteSafeRef.
//
// The installation is idempotent: re-running overwrites the existing hook
// with the current version. The hook file is created with mode 0755 so git
// will execute it.
//
// taskRef is used only for validation (the hook derives the task from the
// branch name at runtime); it must be non-empty.
func (w *WorktreeManager) InstallPreRebaseHook(ctx context.Context, worktreePath, taskRef string) error {
	if strings.TrimSpace(worktreePath) == "" {
		return fmt.Errorf("install pre-rebase hook: worktree path is required")
	}
	if strings.TrimSpace(taskRef) == "" {
		return fmt.Errorf("install pre-rebase hook: task ref is required")
	}

	hooksDir, err := w.worktreeHooksDir(ctx, worktreePath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(hooksDir, 0755); err != nil {
		return fmt.Errorf("install pre-rebase hook: create hooks dir %s: %w", hooksDir, err)
	}
	hookPath := filepath.Join(hooksDir, "pre-rebase")
	if err := os.WriteFile(hookPath, []byte(preRebaseHookScript), 0755); err != nil {
		return fmt.Errorf("install pre-rebase hook: write %s: %w", hookPath, err)
	}
	return nil
}

// worktreeHooksDir resolves the hooks directory where Git will actually
// look for hooks. It uses `git rev-parse --git-path hooks`, which
// respects repo-level core.hooksPath — so the hook is installed where
// git's hook-discovery mechanism finds it, not in a per-worktree
// directory that git never consults (the bug fixed in FAC-214 review).
//
// Global and system config are disabled so a developer's global
// core.hooksPath cannot redirect the hook to an unrelated directory.
// Local config is preserved so a repo-level core.hooksPath is respected.
func (w *WorktreeManager) worktreeHooksDir(ctx context.Context, worktreePath string) (string, error) {
	cmd := execCommandContext(ctx, "git", "-C", worktreePath,
		"rev-parse", "--path-format=absolute", "--git-path", "hooks")
	// Disable global/system config so the developer's global
	// core.hooksPath cannot redirect. Local config is preserved.
	env := make([]string, 0, 64)
	for _, e := range os.Environ() {
		key, _, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(key, "GIT_CONFIG_") {
			env = append(env, e)
		}
	}
	env = append(env,
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_CONFIG_SYSTEM="+os.DevNull,
		"GIT_CONFIG_NOSYSTEM=1",
	)
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("install pre-rebase hook: resolve hooks dir for %s: %w", worktreePath, err)
	}
	hooksDir := strings.TrimSpace(string(out))
	if hooksDir == "" {
		return "", fmt.Errorf("install pre-rebase hook: empty hooks dir for %s", worktreePath)
	}
	return hooksDir, nil
}
