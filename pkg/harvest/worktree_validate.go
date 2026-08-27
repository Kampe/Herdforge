package harvest

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// HarvestSkip names a path that must not be scanned as a harvest candidate.
type HarvestSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ClassifyHarvestInput is the soft pre-filter (scratch / missing / not-a-dir).
// Production harvest uses ClassifyHarvestInputStrict; keep this helper for
// callers that only need the scratch class without a principal-linkage probe.
func ClassifyHarvestInput(repoRoot, path string) (ok bool, reason string) {
	_ = repoRoot
	path = strings.TrimSpace(path)
	if path == "" {
		return false, "empty path"
	}
	if reason := scratchPathReason(path); reason != "" {
		return false, reason
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, "path missing: " + err.Error()
	}
	if !info.IsDir() {
		return false, "not a directory"
	}
	return true, ""
}

// ClassifyHarvestInputStrict is the FAC-604 production gate: soft pre-filter
// plus .git presence and principal common-dir linkage via pkg/gitroot.CommonDir
// (FAC-575: no second --git-common-dir literal). Called from Harvester.harvest
// before any per-path scan work.
func ClassifyHarvestInputStrict(repoRoot, path string) (ok bool, reason string) {
	ok, reason = ClassifyHarvestInput(repoRoot, path)
	if !ok {
		return ok, reason
	}
	gitMeta := filepath.Join(path, ".git")
	if _, err := os.Stat(gitMeta); err != nil {
		return false, "not a git worktree (.git missing)"
	}
	if strings.TrimSpace(repoRoot) == "" {
		return true, ""
	}
	common, err := gitroot.CommonDir(context.Background(), path)
	if err != nil {
		return false, "not a git worktree: " + err.Error()
	}
	principalCommon, err := gitroot.CommonDir(context.Background(), repoRoot)
	if err != nil {
		return true, ""
	}
	commonResolved, err1 := filepath.EvalSymlinks(common)
	principalResolved, err2 := filepath.EvalSymlinks(principalCommon)
	if err1 != nil {
		commonResolved = filepath.Clean(common)
	}
	if err2 != nil {
		principalResolved = filepath.Clean(principalCommon)
	}
	if commonResolved != principalResolved {
		return false, "worktree not linked to principal repository"
	}
	return true, ""
}

func scratchPathReason(path string) string {
	lower := strings.ToLower(filepath.ToSlash(path))
	switch {
	case strings.Contains(lower, "/scratchpad/"):
		return "scratch path (scratchpad directory)"
	case strings.Contains(lower, "/private/tmp/claude-"):
		return "scratch path (claude session temp worktree)"
	case strings.Contains(lower, "/tmp/claude-"):
		return "scratch path (claude session temp worktree)"
	case strings.Contains(lower, "/mcp-nonvacuity"):
		return "scratch path (reviewer non-vacuity check)"
	default:
		return ""
	}
}
