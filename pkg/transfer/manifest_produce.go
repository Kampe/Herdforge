package transfer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
)

// ProduceOptions scope one manifest production pass. Bundles are EXPLICIT
// exact filenames — production never enumerates the root to select bundles,
// because repository location and a .bundle suffix never prove ownership
// (FAC-808). Authority is the operator's explicit declaration under existing
// user authority; nothing here mints cryptographic provenance.
type ProduceOptions struct {
	// RepoRoot must resolve to the canonical repository checkout.
	RepoRoot string
	// Root is the owned bundle directory under `.herd` state.
	Root string
	// Out is the manifest destination. It must be inside Root, and an
	// existing destination is never overwritten.
	Out string
	// Authority is the coordinator's explicit operator declaration, recorded
	// verbatim in the manifest.
	Authority string
	// Bundles lists the exact .bundle filenames to pin.
	Bundles []string
	// Write performs the atomic create. Zero value is a dry-run that only
	// describes the intended manifest.
	Write bool
	// LockDir is the native shared-checkout lock directory. Empty defaults
	// to the canonical repo's `.git/herd-shared-checkout.lock.d`.
	LockDir string
	// LockWait bounds how long production waits for the native lock.
	LockWait time.Duration
}

// ProduceReport is the per-pass receipt.
type ProduceReport struct {
	DryRun  bool             `json:"dry_run"`
	Root    string           `json:"root"`
	Out     string           `json:"out,omitempty"`
	Written bool             `json:"written"`
	Bundles []RetentionEntry `json:"bundles"`
	// ManifestJSON is the exact intended document in dry-run mode and the
	// exact bytes written in write mode.
	ManifestJSON []byte `json:"-"`
}

// ProduceRetentionManifest captures the content identity of explicitly named
// bundles under the native shared-checkout lock and — in write mode —
// atomically creates the version-2 retention manifest the reclaim pass
// consumes. Every refusal is fail-closed: a bundle that cannot be fully
// verified fails the whole pass instead of producing partial authority, and
// the live canonical corpus is never modified (no bundle is deleted, moved,
// or rewritten).
func ProduceRetentionManifest(ctx context.Context, opts ProduceOptions) (ProduceReport, error) {
	report := ProduceReport{DryRun: !opts.Write, Out: opts.Out}
	if strings.TrimSpace(opts.Authority) == "" {
		return report, fmt.Errorf("bundle-manifest: --authority is required: the manifest records the operator's explicit declaration")
	}
	if len(opts.Bundles) == 0 {
		return report, fmt.Errorf("bundle-manifest: at least one --bundle name is required; bundles are never selected implicitly")
	}
	ordered := make([]string, 0, len(opts.Bundles))
	seen := make(map[string]bool, len(opts.Bundles))
	for _, name := range opts.Bundles {
		if !strings.HasSuffix(name, bundleSuffix) || strings.Contains(name, "/") || strings.Contains(name, "\\") || name != filepath.Clean(name) {
			return report, fmt.Errorf("bundle-manifest: %q must be an exact .bundle filename", name)
		}
		if seen[name] {
			return report, fmt.Errorf("bundle-manifest: duplicate bundle name %q", name)
		}
		seen[name] = true
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)

	reclaimOpts := ReclaimOptions{RepoRoot: opts.RepoRoot, Root: opts.Root}
	dir, err := resolveScope(ctx, reclaimOpts)
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: %w", err)
	}
	report.Root = dir
	if strings.TrimSpace(opts.LockDir) == "" {
		opts.LockDir = filepath.Join(opts.RepoRoot, lock.DefaultRelDir)
	}
	lockDirAbs, err := filepath.EvalSymlinks(filepath.Dir(opts.LockDir))
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: native lock parent: %w", err)
	}
	opts.LockDir = filepath.Join(lockDirAbs, filepath.Base(opts.LockDir))
	shared := lock.NewDirLock(opts.LockDir)
	shared.SetMaxAge(bundleReclaimLockMaxAge)
	var lockOwned bool
	if os.Getenv(lock.EnvHeld) != opts.LockDir {
		wait := opts.LockWait
		if wait <= 0 {
			wait = 30 * time.Second
		}
		if err := shared.Acquire(ctx, wait, "herd bundle-manifest serializes identity capture with reclaimers and transfers"); err != nil {
			return report, fmt.Errorf("bundle-manifest: native lock: %w", err)
		}
		lockOwned = true
	}
	if lockOwned {
		defer shared.Release()
	}

	entries := make([]RetentionEntry, 0, len(ordered))
	var totalBytes int64
	for _, name := range ordered {
		entry, err := produceEntry(ctx, opts.RepoRoot, dir, name)
		if err != nil {
			return report, fmt.Errorf("bundle-manifest: %w", err)
		}
		entries = append(entries, entry)
		totalBytes += entry.Size
	}
	manifest := RetentionManifest{Version: 2, Authority: opts.Authority, Bundles: entries}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return report, err
	}
	body = append(body, '\n')
	report.Bundles = entries
	report.ManifestJSON = body

	if !opts.Write {
		fmt.Fprintln(os.Stderr, string(body))
		fmt.Fprintf(os.Stderr, "herd bundle-manifest: dry run describing %d bundle(s), %d bytes; no file was created\n", len(entries), totalBytes)
		return report, nil
	}

	written, err := createManifestAtomically(dir, opts.Out, body)
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: %w", err)
	}
	report.Out = written
	report.Written = true
	fmt.Fprintf(os.Stderr, "herd bundle-manifest: wrote %s (%d bundle(s), %d bytes pinned); no bundle was modified or deleted\n", written, len(entries), totalBytes)
	return report, nil
}

// produceEntry captures one bundle's full content identity under the guards
// the reclaim pass re-checks at deletion time, so every produced entry is
// exactly reproducible: same size, same integer-nanosecond modification
// time, same streaming sha256, and a regular single-linked non-symlink file
// whose git tips are verified and retained by canonical refs.
func produceEntry(ctx context.Context, repoRoot, dir, name string) (RetentionEntry, error) {
	path := filepath.Join(dir, name)
	st, err := os.Lstat(path)
	if err != nil {
		return RetentionEntry{}, fmt.Errorf("bundle %s: %w", name, err)
	}
	if !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return RetentionEntry{}, fmt.Errorf("bundle %s: not a regular non-symlink file", name)
	}
	if fileLinkCount(st) != 1 {
		return RetentionEntry{}, fmt.Errorf("bundle %s: hard-link-substitution-possible (link count %d)", name, fileLinkCount(st))
	}
	if st.Size() <= 0 {
		return RetentionEntry{}, fmt.Errorf("bundle %s: non-positive size", name)
	}
	if !verifyBundle(ctx, repoRoot, path) {
		return RetentionEntry{}, fmt.Errorf("bundle %s: bundle-verify-failed", name)
	}
	tips, err := listBundleTips(ctx, path)
	if err != nil || len(tips) == 0 {
		return RetentionEntry{}, fmt.Errorf("bundle %s: bundle-tips-unknown: %v", name, err)
	}
	if reason := containmentReason(ctx, repoRoot, tips); reason != "" {
		return RetentionEntry{}, fmt.Errorf("bundle %s: %s", name, reason)
	}
	digest, err := fileContentDigest(path, st.Size())
	if err != nil {
		return RetentionEntry{}, fmt.Errorf("bundle %s: identity capture failed: %w", name, err)
	}
	after, err := os.Lstat(path)
	if err != nil {
		return RetentionEntry{}, fmt.Errorf("bundle %s: identity recheck failed: %w", name, err)
	}
	if !os.SameFile(st, after) || after.Size() != st.Size() || after.ModTime().UnixNano() != st.ModTime().UnixNano() {
		return RetentionEntry{}, fmt.Errorf("bundle %s: changed during identity capture; the pinned identity would not reproduce at deletion", name)
	}
	return RetentionEntry{
		Name:            name,
		Digest:          digest,
		Size:            st.Size(),
		ModTimeUnixNano: st.ModTime().UnixNano(),
	}, nil
}

// createManifestAtomically creates opts.Out inside dir with O_EXCL — an
// existing artifact is never overwritten, and a symlink destination fails
// instead of being followed. The destination's parent is resolved through
// symlinks and must stay inside the owned root, so a parent swap cannot
// smuggle the write outside it.
func createManifestAtomically(dir, out string, body []byte) (string, error) {
	if strings.TrimSpace(out) == "" {
		return "", fmt.Errorf("--out is required for writing")
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return "", fmt.Errorf("output destination: %w", err)
	}
	parent := filepath.Dir(absOut)
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("output destination: %w", err)
	}
	rel, err := filepath.Rel(dir, realParent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("output destination %s is not inside the owned root", out)
	}
	if _, err := os.Lstat(absOut); err == nil || !os.IsNotExist(err) {
		if err == nil {
			return "", fmt.Errorf("output %s already exists; the manifest is never overwritten", out)
		}
		return "", fmt.Errorf("output %s: %w", out, err)
	}
	if st, err := os.Lstat(realParent); err != nil || !st.IsDir() {
		return "", fmt.Errorf("output destination parent must be a real directory inside the owned root")
	}
	target := filepath.Join(realParent, filepath.Base(absOut))
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return "", fmt.Errorf("atomic create refused: %w", err)
	}
	if _, err := f.Write(body); err != nil {
		f.Close()
		return "", fmt.Errorf("atomic create failed: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return "", fmt.Errorf("atomic create failed: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("atomic create failed: %w", err)
	}
	if st, err := os.Lstat(target); err != nil || !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("output %s is not a regular non-symlink file after create", out)
	}
	return target, nil
}
