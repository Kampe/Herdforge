// Package worktreebootstrap executes the repository-declared task-worktree
// bootstrap contract. It owns only per-worktree derived artifacts; it never
// writes user-level tool caches or configuration.
package worktreebootstrap

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
)

const receiptVersion = 1

type Receipt struct {
	Version         int    `json:"version"`
	ContractDigest  string `json:"contract_digest"`
	ToolchainDigest string `json:"toolchain_digest"`
	CacheDir        string `json:"cache_dir"`
	RuntimeDir      string `json:"runtime_dir"`
}

type Result struct {
	Receipt Receipt
	Reused  bool
}

type ToolchainResolver interface {
	Resolve(context.Context, string) (string, error)
}

type CommandRunner interface {
	Run(context.Context, string, []string, []string) error
}

type Executor struct {
	Resolver ToolchainResolver
	Runner   CommandRunner
}

type execResolver struct{}

func (execResolver) Resolve(_ context.Context, name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s|%d|%d", resolved, info.Size(), info.ModTime().UTC().UnixNano()), nil
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, dir string, argv, env []string) error {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return errors.New("bootstrap command is empty")
	}
	command := argv[0]
	if strings.ContainsRune(command, filepath.Separator) {
		var err error
		command, err = safeChild(dir, command)
		if err != nil {
			return fmt.Errorf("bootstrap command path: %w", err)
		}
	}
	cmd := exec.CommandContext(ctx, command, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("bootstrap command %q: %w: %s", argv[0], err, strings.TrimSpace(string(output)))
	}
	return nil
}

func (e Executor) Execute(ctx context.Context, worktreePath string, contract config.WorktreeBootstrap) (*Result, error) {
	if !contract.Enabled() {
		return &Result{}, nil
	}
	if err := contract.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(worktreePath) == "" {
		return nil, errors.New("bootstrap worktree path is required")
	}
	root, err := filepath.Abs(worktreePath)
	if err != nil {
		return nil, fmt.Errorf("resolve bootstrap worktree: %w", err)
	}
	if resolved, resolveErr := filepath.EvalSymlinks(root); resolveErr == nil {
		root = resolved
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("bootstrap worktree unavailable: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("bootstrap worktree unavailable: %s is not a directory", root)
	}
	resolver := e.Resolver
	if resolver == nil {
		resolver = execResolver{}
	}
	identity, err := resolver.Resolve(ctx, contract.Toolchain)
	if err != nil {
		return nil, fmt.Errorf("resolve bootstrap toolchain %q: %w", contract.Toolchain, err)
	}
	if strings.TrimSpace(identity) == "" {
		return nil, fmt.Errorf("resolve bootstrap toolchain %q: empty identity", contract.Toolchain)
	}
	contractDigest := digest(contract.Version + "\x00" + contract.Toolchain + "\x00" + strings.Join(contract.Command, "\x00"))
	toolchainDigest := digest(identity)
	cacheRel := filepath.ToSlash(filepath.Join(".herd", "bootstrap", "cache", toolchainDigest))
	runtimeRel := filepath.ToSlash(filepath.Join(".herd", "bootstrap", "runtime", toolchainDigest))
	want := Receipt{Version: receiptVersion, ContractDigest: contractDigest, ToolchainDigest: toolchainDigest, CacheDir: cacheRel, RuntimeDir: runtimeRel}

	receiptPath := filepath.Join(root, ".herd", "bootstrap", "receipt.json")
	if current, exists, err := readReceipt(receiptPath); err != nil {
		return nil, err
	} else if exists && current == want && directoriesExist(root, want) {
		return &Result{Receipt: want, Reused: true}, nil
	} else if exists {
		if err := repair(root, current); err != nil {
			return nil, err
		}
	} else if err := repair(root, want); err != nil {
		// A prior failed run has no success receipt, so the derived paths are
		// untrusted partial artifacts. Repair them before retrying.
		return nil, err
	}
	cachePath, err := safeChild(root, cacheRel)
	if err != nil {
		return nil, err
	}
	runtimePath, err := safeChild(root, runtimeRel)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cachePath, 0o700); err != nil {
		return nil, fmt.Errorf("create bootstrap cache: %w", err)
	}
	if err := os.MkdirAll(runtimePath, 0o700); err != nil {
		return nil, fmt.Errorf("create bootstrap runtime: %w", err)
	}
	runner := e.Runner
	if runner == nil {
		runner = execRunner{}
	}
	if err := runner.Run(ctx, root, append([]string(nil), contract.Command...), bootstrapEnv(cachePath, runtimePath)); err != nil {
		return nil, err
	}
	if err := writeReceipt(receiptPath, want); err != nil {
		return nil, err
	}
	return &Result{Receipt: want}, nil
}

func digest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func bootstrapEnv(cachePath, runtimePath string) []string {
	env := append([]string(nil), os.Environ()...)
	return append(env,
		"HERD_BOOTSTRAP_CACHE="+cachePath,
		"HERD_BOOTSTRAP_RUNTIME="+runtimePath,
		"GOCACHE="+filepath.Join(cachePath, "go-build"),
		"GOMODCACHE="+filepath.Join(cachePath, "go-mod"),
		"TMPDIR="+filepath.Join(runtimePath, "tmp"),
	)
}

func readReceipt(path string) (Receipt, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Receipt{}, false, nil
	}
	if err != nil {
		return Receipt{}, false, fmt.Errorf("read bootstrap receipt: %w", err)
	}
	var receipt Receipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		return Receipt{}, false, fmt.Errorf("bootstrap receipt is invalid: %w", err)
	}
	if receipt.Version != receiptVersion || receipt.ContractDigest == "" || receipt.ToolchainDigest == "" {
		return Receipt{}, false, errors.New("bootstrap receipt is incomplete")
	}
	return receipt, true, nil
}

func directoriesExist(root string, receipt Receipt) bool {
	for _, rel := range []string{receipt.CacheDir, receipt.RuntimeDir} {
		path, err := safeChild(root, rel)
		if err != nil {
			return false
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

func repair(root string, receipt Receipt) error {
	for _, rel := range []string{receipt.CacheDir, receipt.RuntimeDir} {
		path, err := safeChild(root, rel)
		if err != nil {
			return fmt.Errorf("bootstrap receipt artifact invalid: %w", err)
		}
		if err := removeOwnedTree(path); err != nil {
			return fmt.Errorf("repair bootstrap artifact: %w", err)
		}
	}
	return nil
}

// removeOwnedTree makes only the declared artifact root removable. WalkDir
// does not follow symlinks, so a cache artifact cannot cause chmod to reach
// outside the worktree or the authenticated artifact root.
func removeOwnedTree(path string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("bootstrap artifact is a symlink: %q", path)
	}
	if err := filepath.WalkDir(path, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fs.SkipDir
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0o200
		if entry.IsDir() {
			mode |= 0o100
		}
		return os.Chmod(current, mode) // #nosec G122 -- the path is yielded by the bounded bootstrap WalkDir.
	}); err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func safeChild(root, rel string) (string, error) {
	if filepath.IsAbs(rel) || strings.HasPrefix(filepath.Clean(rel), ".."+string(filepath.Separator)) || filepath.Clean(rel) == ".." {
		return "", fmt.Errorf("bootstrap artifact path is not worktree-relative: %q", rel)
	}
	path := filepath.Join(root, rel)
	base := filepath.Clean(root) + string(filepath.Separator)
	if !strings.HasPrefix(filepath.Clean(path)+string(filepath.Separator), base) {
		return "", fmt.Errorf("bootstrap artifact escapes worktree: %q", rel)
	}
	for current := filepath.Dir(path); current != root; current = filepath.Dir(current) {
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("inspect bootstrap artifact parent: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("bootstrap artifact parent is a symlink: %q", current)
		}
	}
	return path, nil
}

func writeReceipt(path string, receipt Receipt) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create bootstrap receipt directory: %w", err)
	}
	data, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		return fmt.Errorf("encode bootstrap receipt: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt-*")
	if err != nil {
		return fmt.Errorf("create bootstrap receipt: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write bootstrap receipt: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod bootstrap receipt: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync bootstrap receipt: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close bootstrap receipt: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("commit bootstrap receipt: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err == nil {
		defer dir.Close()
		if err := dir.Sync(); err != nil {
			return fmt.Errorf("sync bootstrap receipt directory: %w", err)
		}
	}
	return nil
}
