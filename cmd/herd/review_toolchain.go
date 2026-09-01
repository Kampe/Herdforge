package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const (
	envReviewGit = worktree.EnvReviewGit
	envReviewGo  = "HERD_REVIEW_GO"
)

// reviewToolchain is the exact machine-local tool identity admitted for a W4
// review. These paths are observed and propagated; Herdforge never installs or
// rewrites the host's Git or Go distribution.
type reviewToolchain struct {
	GitPath    string
	GitVersion string
	GoPath     string
	GoVersion  string
	GoRoot     string
	GoToolDir  string
}

// preflightReviewToolchain proves the exact Git and Go capabilities needed by
// a W4 reviewer before a warm-pool lease or reviewer tab can exist.
func preflightReviewToolchain(repoRoot string) (reviewToolchain, error) {
	gitPath, err := resolveReviewExecutable("git", strings.TrimSpace(os.Getenv(envReviewGit)))
	if err != nil {
		return reviewToolchain{}, reviewToolRefusal("required git is unavailable", err)
	}
	gitVersion, gitVersionErr := reviewToolOutput(gitPath, nil, "--version")
	if gitVersionErr != nil {
		return reviewToolchain{}, reviewToolRefusal(
			fmt.Sprintf("required git at %s cannot report its version", gitPath), gitVersionErr)
	}
	capability := "merge-tree " + gitroot.MergeTreeWriteFlag + " " + gitroot.MergeTreeHeadBaseFlag + " HEAD HEAD"
	if _, capabilityErr := reviewToolOutput(gitPath, nil,
		"-C", repoRoot, "merge-tree", gitroot.MergeTreeWriteFlag, gitroot.MergeTreeHeadBaseFlag, "HEAD", "HEAD"); capabilityErr != nil {
		return reviewToolchain{}, reviewToolRefusal(
			fmt.Sprintf("git path=%s version=%q is missing required capability %q", gitPath, gitVersion, capability),
			capabilityErr)
	}

	goPath, err := resolveReviewExecutable("go", strings.TrimSpace(os.Getenv(envReviewGo)))
	if err != nil {
		return reviewToolchain{}, reviewToolRefusal("required go is unavailable", err)
	}
	goEnv := reviewGoProbeEnv()
	goVersion, goVersionErr := reviewToolOutput(goPath, goEnv, "version")
	if goVersionErr != nil {
		return reviewToolchain{}, reviewToolRefusal(
			fmt.Sprintf("required go at %s cannot report its version", goPath), goVersionErr)
	}
	goRoots, goEnvErr := reviewToolOutput(goPath, goEnv, "env", "GOROOT", "GOTOOLDIR")
	if goEnvErr != nil {
		return reviewToolchain{}, reviewToolRefusal(
			fmt.Sprintf("required go at %s version=%q cannot resolve GOROOT/GOTOOLDIR", goPath, goVersion), goEnvErr)
	}
	rootLines := strings.Split(strings.TrimSpace(goRoots), "\n")
	if len(rootLines) != 2 || strings.TrimSpace(rootLines[0]) == "" || strings.TrimSpace(rootLines[1]) == "" {
		return reviewToolchain{}, reviewToolRefusal(
			fmt.Sprintf("required go at %s version=%q returned incomplete GOROOT/GOTOOLDIR identity %q", goPath, goVersion, goRoots), nil)
	}

	toolchain := reviewToolchain{
		GitPath: gitPath, GitVersion: gitVersion,
		GoPath: goPath, GoVersion: goVersion,
		GoRoot: strings.TrimSpace(rootLines[0]), GoToolDir: strings.TrimSpace(rootLines[1]),
	}
	fmt.Printf("review toolchain ready git_path=%s git_version=%q git_capability=%q go_path=%s go_version=%q\n",
		toolchain.GitPath, toolchain.GitVersion, capability, toolchain.GoPath, toolchain.GoVersion)
	return toolchain, nil
}

func reviewToolRefusal(reason string, cause error) error {
	const action = "No lease or tab was created. Configure exact machine-local binaries with HERD_REVIEW_GIT and HERD_REVIEW_GO; Herdforge will not install or modify a foreign package manager."
	if cause == nil {
		return fmt.Errorf("review toolchain refused: %s. %s", reason, action)
	}
	return fmt.Errorf("review toolchain refused: %s: %w. %s", reason, cause, action)
}

func resolveReviewExecutable(name, configured string) (string, error) {
	path := configured
	if path == "" {
		resolved, err := exec.LookPath(name)
		if err != nil {
			return "", fmt.Errorf("resolve %s on PATH: %w", name, err)
		}
		path = resolved
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s path %q: %w", name, path, err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("inspect %s path %q: %w", name, abs, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s path %q is not a regular file", name, abs)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%s path %q is not executable", name, abs)
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(abs)), ".exe")
	if base != name {
		return "", fmt.Errorf("%s path %q must name a %s executable so reviewer PATH resolves the admitted binary", name, abs, name)
	}
	return filepath.Clean(abs), nil
}

func reviewToolOutput(path string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	if env != nil {
		cmd.Env = env
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s %s: %w", path, strings.Join(args, " "), ctx.Err())
		}
		return "", fmt.Errorf("%s %s: %v (%s)", path, strings.Join(args, " "), err, detail)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func reviewGoProbeEnv() []string {
	base := os.Environ()
	env := make([]string, 0, len(base)+1)
	for _, entry := range base {
		key, _, _ := strings.Cut(entry, "=")
		if key == "GOROOT" || key == "GOTOOLDIR" || key == "GOTOOLCHAIN" {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GOTOOLCHAIN=local")
}

// reviewerTabEnvironment binds the actual pane process to the binaries that
// passed admission. PATH is explicit because the long-lived Herdr server may
// have a different startup environment than the launcher shell.
func reviewerTabEnvironment(toolchain reviewToolchain) []string {
	path := prependReviewToolDirs(os.Getenv("PATH"), filepath.Dir(toolchain.GitPath), filepath.Dir(toolchain.GoPath))
	return []string{
		herdr.AgentRoleEnv,
		envReviewGit + "=" + toolchain.GitPath,
		envReviewGo + "=" + toolchain.GoPath,
		"PATH=" + path,
		"GOROOT=" + toolchain.GoRoot,
		"GOTOOLDIR=" + toolchain.GoToolDir,
		"GOTOOLCHAIN=local",
	}
}

func prependReviewToolDirs(existing string, dirs ...string) string {
	seen := make(map[string]bool, len(dirs))
	parts := make([]string, 0, len(dirs)+1)
	for _, dir := range dirs {
		dir = strings.TrimSpace(dir)
		if dir == "" || seen[dir] {
			continue
		}
		seen[dir] = true
		parts = append(parts, dir)
	}
	if existing != "" {
		parts = append(parts, existing)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func reviewGitCommand() string {
	if path := strings.TrimSpace(os.Getenv(envReviewGit)); path != "" {
		return path
	}
	return "git"
}
