package harvest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// HarvestSkip names a path that must not be scanned as a harvest candidate.
type HarvestSkip struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// ClassifyHarvestInput decides whether a path should enter harvest/drain
// scanning (FAC-604). Scratch and missing paths are excluded with a named
// reason. Linked-worktree verification is intentional soft: capacity-gate and
// command-stub tests invent paths that are not real git trees, and those must
// still reach the existing admission/checkUnmerged path. Residual
// "not a git worktree" failures are reclassified as exclusions at the drain
// printer, never UNKNOWN.
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

// ClassifyHarvestInputStrict adds principal-linkage checks for unit tests and
// callers that need a hard worktree proof.
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
	common, err := gitCommonDir(path)
	if err != nil {
		return false, "not a git worktree: " + err.Error()
	}
	principalCommon, err := gitCommonDir(repoRoot)
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

func gitCommonDir(path string) (string, error) {
	cmd := exec.Command("git", "-C", path, "rev-parse", "--git-common-dir")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	common := strings.TrimSpace(string(out))
	if common == "" {
		return "", fmt.Errorf("empty common dir")
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(path, common)
	}
	return filepath.Clean(common), nil
}
