package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

// migratedWorktree is deliberately path-free in durable output. Absolute
// paths are useful only to the executing git command.
type migratedWorktree struct {
	ID     string `json:"id"`
	Branch string `json:"branch,omitempty"`
	From   string `json:"from"`
	To     string `json:"to"`
	Action string `json:"action"`
}

// runWorktreeMigrate moves legacy registered worktrees into the managed state
// root. It is dry-run by default. git worktree move preserves both committed
// and uncommitted work, so migration never guesses that old work is disposable.
func runWorktreeMigrate(args []string, out, errOut io.Writer) error {
	fs := flag.NewFlagSet("worktree-migrate", flag.ContinueOnError)
	fs.SetOutput(errOut)
	apply := fs.Bool("apply", false, "move legacy worktrees into the state root")
	jsonOut := fs.Bool("json", false, "emit the migration plan as JSON")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("worktree-migrate: unexpected argument %q", fs.Arg(0))
	}
	root, err := worktree.ResolveCanonicalRoot(context.Background(), ".", firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ""))
	if err != nil {
		return fmt.Errorf("worktree-migrate: resolve canonical repository root: %w", err)
	}
	entries, err := worktree.NewWorktreeManager(root).ListWorktrees(context.Background())
	if err != nil {
		return fmt.Errorf("worktree-migrate: list registered worktrees: %w", err)
	}
	managed := worktree.NewStateWorktreeManager(root)
	stateRoot := worktree.StateWorktreeRoot(root)
	plan := make([]migratedWorktree, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || sameMigrationPath(entry.Path, root) || withinMigrationRoot(stateRoot, entry.Path) {
			continue
		}
		id := migrationID(entry.Path)
		target := filepath.Join(stateRoot, "legacy", id)
		row := migratedWorktree{ID: id, Branch: entry.Branch, From: migrationDisplay(entry.Path, root), To: filepath.ToSlash(filepath.Join("legacy", id)), Action: "move"}
		plan = append(plan, row)
		if !*apply {
			continue
		}
		if _, statErr := os.Lstat(target); statErr == nil {
			return fmt.Errorf("worktree-migrate: target %s already exists", row.To)
		} else if !os.IsNotExist(statErr) {
			return fmt.Errorf("worktree-migrate: inspect target %s: %w", row.To, statErr)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return fmt.Errorf("worktree-migrate: create managed state root: %w", err)
		}
		cmd := exec.CommandContext(context.Background(), "git", "-C", root, "worktree", "move", "--", entry.Path, target)
		if output, moveErr := cmd.CombinedOutput(); moveErr != nil {
			return fmt.Errorf("worktree-migrate: move %s: %v: %s", row.From, moveErr, strings.TrimSpace(string(output)))
		}
		if err := managed.MigrateRecord("legacy", id, "migration", entry.Commit, target); err != nil {
			return fmt.Errorf("worktree-migrate: record %s: %w", id, err)
		}
	}
	sort.Slice(plan, func(i, j int) bool { return plan[i].ID < plan[j].ID })
	if *jsonOut {
		return json.NewEncoder(out).Encode(map[string]any{"apply": *apply, "worktrees": plan})
	}
	mode := "DRY RUN"
	if *apply {
		mode = "APPLIED"
	}
	fmt.Fprintf(out, "worktree-migrate: %s %d legacy worktree(s)\n", mode, len(plan))
	for _, item := range plan {
		fmt.Fprintf(out, "  %s %s -> %s\n", item.Action, item.From, item.To)
	}
	if !*apply {
		fmt.Fprintln(out, "worktree-migrate: dry-run only; pass --apply to move the listed worktrees")
	}
	return nil
}

func migrationID(path string) string {
	base := strings.ToLower(filepath.Base(filepath.Clean(path)))
	base = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			return r
		}
		return '-'
	}, base)
	base = strings.Trim(base, "-")
	if base == "" {
		base = "worktree"
	}
	digest := sha256.Sum256([]byte(filepath.Clean(path)))
	return base + "-" + hex.EncodeToString(digest[:])[:12]
}

func sameMigrationPath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	if resolved, err := filepath.EvalSymlinks(aa); err == nil {
		aa = resolved
	}
	if resolved, err := filepath.EvalSymlinks(bb); err == nil {
		bb = resolved
	}
	return filepath.Clean(aa) == filepath.Clean(bb)
}

func withinMigrationRoot(root, path string) bool {
	rootAbs, _ := filepath.Abs(root)
	pathAbs, _ := filepath.Abs(path)
	if resolved, err := filepath.EvalSymlinks(rootAbs); err == nil {
		rootAbs = resolved
	}
	if resolved, err := filepath.EvalSymlinks(pathAbs); err == nil {
		pathAbs = resolved
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func migrationDisplay(path, root string) string {
	if rel, err := filepath.Rel(root, path); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "./" + filepath.ToSlash(rel)
	}
	return "legacy/" + migrationID(path)
}
