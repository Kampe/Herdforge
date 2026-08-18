package harvestmerge

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

// UnionMergeConfig names repository-relative, append-only files that harvest
// merge may union when two lanes add different content to the same file.
type UnionMergeConfig struct {
	Paths []string
}

// Enabled reports whether path is one of the explicitly configured paths.
// Paths are intentionally exact and repository-relative: broad globs or
// absolute paths would make a merge gate silently alter unrelated files.
func (c UnionMergeConfig) Enabled(path string) bool {
	if path == "" || filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, "../") || path == ".." {
		return false
	}
	for _, configured := range c.Paths {
		if path == configured {
			return true
		}
	}
	return false
}

// UnionMerge combines two append-only versions of a common base. The base
// content must be an exact prefix of both versions; otherwise the caller must
// leave the file as a normal conflict. Distinct lane suffixes are retained in
// deterministic argument order and duplicate suffixes are emitted once.
func UnionMerge(base, ours, theirs string) (string, error) {
	if !strings.HasPrefix(ours, base) || !strings.HasPrefix(theirs, base) {
		return "", errors.New("union merge requires both lanes to preserve the base prefix")
	}
	oursSuffix, theirsSuffix := strings.TrimPrefix(ours, base), strings.TrimPrefix(theirs, base)
	if oursSuffix == "" {
		return theirs, nil
	}
	if theirsSuffix == "" || oursSuffix == theirsSuffix {
		return ours, nil
	}
	return base + oursSuffix + theirsSuffix, nil
}

// MatrixIDAllocator serializes allocation in a small append-only file. The
// lock is a sibling file so readers can inspect the ledger without parsing a
// lock record. A process crash releases the advisory lock automatically.
type MatrixIDAllocator struct {
	Path   string
	Prefix string
}

func NewMatrixIDAllocator(path, prefix string) MatrixIDAllocator {
	return MatrixIDAllocator{Path: path, Prefix: prefix}
}

func (a MatrixIDAllocator) Next() (string, error) {
	if strings.TrimSpace(a.Path) == "" || strings.TrimSpace(a.Prefix) == "" {
		return "", errors.New("matrix ID allocator requires a path and prefix")
	}
	if strings.ContainsAny(a.Prefix, "\r\n") {
		return "", errors.New("matrix ID prefix must not contain newlines")
	}
	if err := os.MkdirAll(filepath.Dir(a.Path), 0o700); err != nil {
		return "", fmt.Errorf("create matrix ID directory: %w", err)
	}
	lock, err := os.OpenFile(a.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", fmt.Errorf("open matrix ID lock: %w", err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return "", fmt.Errorf("lock matrix ID ledger: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	next := 1
	file, err := os.Open(a.Path)
	if err == nil {
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if !strings.HasPrefix(line, a.Prefix+"-") {
				continue
			}
			n, parseErr := strconv.Atoi(strings.TrimPrefix(line, a.Prefix+"-"))
			if parseErr == nil && n >= next {
				next = n + 1
			}
		}
		scanErr := scanner.Err()
		file.Close()
		if scanErr != nil {
			return "", fmt.Errorf("read matrix ID ledger: %w", scanErr)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("open matrix ID ledger: %w", err)
	}

	id := fmt.Sprintf("%s-%d", a.Prefix, next)
	file, err = os.OpenFile(a.Path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return "", fmt.Errorf("open matrix ID ledger for append: %w", err)
	}
	_, writeErr := fmt.Fprintln(file, id)
	closeErr := file.Close()
	if writeErr != nil {
		return "", fmt.Errorf("write matrix ID ledger: %w", writeErr)
	}
	if closeErr != nil {
		return "", fmt.Errorf("close matrix ID ledger: %w", closeErr)
	}
	return id, nil
}

// AllocateMatrixID is the concise one-shot allocator for callers that do not
// need to retain an allocator instance.
func AllocateMatrixID(path, prefix string) (string, error) {
	return NewMatrixIDAllocator(path, prefix).Next()
}

// SortedPaths returns a copy suitable for deterministic command construction.
func (c UnionMergeConfig) SortedPaths() []string {
	paths := append([]string(nil), c.Paths...)
	sort.Strings(paths)
	return paths
}
