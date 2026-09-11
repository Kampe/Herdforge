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
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// Producer bounds are reused from the reclaim pass: a produced manifest can
// never admit more entries than one bounded reclaim pass may enumerate, nor
// pin more bytes than one bounded pass may reclaim. Callers may not raise
// them via flags; tests may lower them.
var (
	produceMaxEntries    = DefaultMaxFiles
	produceMaxTotalBytes = DefaultMaxReclaimBytes
)

// Deterministic test seams for failure/interleave injection. Production
// leaves both nil.
var (
	// produceTempWriteHook fires after the owned temporary file exists and
	// before the body is written; a non-nil error aborts with cleanup.
	produceTempWriteHook func(tempPath string) error
	// producePublishHook fires after the temporary file is complete and
	// before the no-replace publication, so tests can swap the output
	// parent or destination concurrently.
	producePublishHook func(tempPath, finalPath string)
	// produceLockRevalidateHook fires after the native lock is acquired
	// and before the pinned owned-root revalidation, so tests can swap the
	// root while the pass was blocked on the lock.
	produceLockRevalidateHook func()
	// produceTempStat is the identity read of the owned temporary file;
	// tests inject failure here to exercise the post-create cleanup path.
	produceTempStat = defaultTempStat
	// produceSyncDirectory establishes durable directory-entry publication
	// for the output parent; tests inject failure here to prove the
	// partial-publication error and published-file preservation.
	produceSyncDirectory = defaultSyncDirectory
)

func defaultTempStat(f *os.File) (os.FileInfo, error) {
	return f.Stat()
}

// defaultSyncDirectory opens the directory and fsyncs it so the published
// entry survives a crash; the open and the sync are both load-bearing.
func defaultSyncDirectory(dir string) error {
	dirFile, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dirFile.Close()
	return dirFile.Sync()
}

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
	// Write performs the atomic publication. Zero value is a dry-run that
	// only describes the intended manifest.
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
	// exact bytes published in write mode.
	ManifestJSON []byte `json:"-"`
}

// ProduceRetentionManifest captures the content identity of explicitly named
// bundles under the native shared-checkout lock and — in write mode —
// publishes the version-2 retention manifest the reclaim pass consumes
// through a same-parent temporary file and a no-replace atomic publication.
// Every refusal is fail-closed: a bundle that cannot be fully verified fails
// the whole pass instead of producing partial authority, and the live
// canonical corpus is never modified (no bundle is deleted, moved, or
// rewritten).
func ProduceRetentionManifest(ctx context.Context, opts ProduceOptions) (ProduceReport, error) {
	report := ProduceReport{DryRun: !opts.Write, Out: opts.Out}
	if strings.TrimSpace(opts.Authority) == "" {
		return report, fmt.Errorf("bundle-manifest: --authority is required: the manifest records the operator's explicit declaration")
	}
	if len(opts.Bundles) == 0 {
		return report, fmt.Errorf("bundle-manifest: at least one --bundle name is required; bundles are never selected implicitly")
	}
	if len(opts.Bundles) > produceMaxEntries {
		return report, fmt.Errorf("bundle-manifest: %d entries exceed the manifest bound of %d (the reclaim enumeration budget)", len(opts.Bundles), produceMaxEntries)
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
	// Pin the owned root's identity BEFORE the lock wait, so a root swapped
	// while this pass blocked on the lock is refused rather than trusted.
	rootInfo, err := os.Lstat(dir)
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: owned root identity: %w", err)
	}
	if strings.TrimSpace(opts.LockDir) == "" {
		// The shared-checkout lock serializes every worktree of the
		// canonical repository, so the default must live in the canonical
		// COMMON .git — never in a linked worktree's .git pointer file.
		canonLock, err := worktree.ResolveCanonicalRoot(ctx, opts.RepoRoot, "")
		if err != nil {
			return report, fmt.Errorf("bundle-manifest: canonical repository identity for the shared lock: %w", err)
		}
		opts.LockDir = filepath.Join(canonLock, lock.DefaultRelDir)
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
	if produceLockRevalidateHook != nil {
		produceLockRevalidateHook()
	}
	// Revalidate the pinned root under the lock: the identity that was
	// scoped before the wait must still own the path that will be read.
	lockedInfo, err := os.Lstat(dir)
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: owned root identity under lock: %w", err)
	}
	if !os.SameFile(rootInfo, lockedInfo) {
		return report, fmt.Errorf("bundle-manifest: owned root replaced while acquiring the native lock")
	}
	// Descriptor-anchored pin: the open directory handle keeps the true
	// inode even if the path is later swapped, so publication revalidation
	// compares fresh path identity against the descriptor, not against a
	// stale pathname assumption.
	rootHandle, err := os.Open(dir)
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: owned root descriptor: %w", err)
	}
	defer rootHandle.Close()
	rootHandleInfo, err := rootHandle.Stat()
	if err != nil {
		return report, fmt.Errorf("bundle-manifest: owned root descriptor identity: %w", err)
	}

	entries := make([]RetentionEntry, 0, len(ordered))
	var totalBytes int64
	for _, name := range ordered {
		entry, err := produceEntry(ctx, opts.RepoRoot, dir, name)
		if err != nil {
			return report, fmt.Errorf("bundle-manifest: %w", err)
		}
		if entry.Size > produceMaxTotalBytes || totalBytes > produceMaxTotalBytes-entry.Size {
			return report, fmt.Errorf("bundle-manifest: pinned bytes would exceed the manifest bound of %d (the reclaim byte budget)", produceMaxTotalBytes)
		}
		totalBytes += entry.Size
		entries = append(entries, entry)
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

	written, err := publishManifestAtomically(dir, rootHandleInfo, opts.Out, body)
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
	if modTimeNano := st.ModTime().UnixNano(); modTimeNano <= 0 {
		// The v2 consumer refuses ModTimeUnixNano <= 0 (LoadRetentionManifest),
		// so an entry with missing or pre-epoch metadata must fail HERE —
		// the producer may never emit a document its own consumer rejects.
		return RetentionEntry{}, fmt.Errorf("bundle %s: invalid modification time %d; the v2 consumer requires a positive integer nanosecond identity", name, modTimeNano)
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

// publishManifestAtomically publishes the manifest body at out inside the
// owned root without ever exposing partial output at the destination: the
// body is written, synced, and closed in a same-parent owned temporary
// file, then published through os.Link — a no-replace atomic primitive that
// fails with EEXIST when the destination exists, on every supported
// platform. Only the owned temporary file is ever cleaned up.
func publishManifestAtomically(rootDir string, rootHandleInfo os.FileInfo, out string, body []byte) (string, error) {
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
	rel, err := filepath.Rel(rootDir, realParent)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", fmt.Errorf("output destination %s is not inside the owned root", out)
	}
	// Pin the output parent's identity now and revalidate it at
	// publication: a directory swapped in between capture and publish is
	// refused instead of receiving manifest authority.
	parentInfo, err := os.Lstat(realParent)
	if err != nil || !parentInfo.IsDir() {
		return "", fmt.Errorf("output destination parent must be a real directory inside the owned root")
	}
	// The owned root itself must still be the descriptor-pinned inode at
	// publication time.
	rootNow, err := os.Lstat(rootDir)
	if err != nil {
		return "", fmt.Errorf("owned root identity at publication: %w", err)
	}
	if !os.SameFile(rootHandleInfo, rootNow) {
		return "", fmt.Errorf("owned root replaced between identity capture and publication")
	}

	temp, err := os.CreateTemp(realParent, ".herd-bundle-manifest-*.tmp")
	if err != nil {
		return "", fmt.Errorf("owned temporary file: %w", err)
	}
	tempPath := temp.Name()
	// Register the owned-temporary cleanup before any post-create failure
	// path: every return from here on removes the owned temporary pathname
	// and never touches the destination.
	defer func() {
		// Clean only the owned temporary file; the destination is never
		// removed or replaced by cleanup.
		_ = os.Remove(tempPath)
	}()
	tempInfo, err := produceTempStat(temp)
	if err != nil {
		temp.Close()
		return "", fmt.Errorf("owned temporary file identity: %w", err)
	}
	if produceTempWriteHook != nil {
		if hookErr := produceTempWriteHook(tempPath); hookErr != nil {
			return "", fmt.Errorf("temporary write aborted: %w", hookErr)
		}
	}
	if _, err := temp.Write(body); err != nil {
		return "", fmt.Errorf("temporary write failed: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return "", fmt.Errorf("temporary sync failed: %w", err)
	}
	if err := temp.Close(); err != nil {
		return "", fmt.Errorf("temporary close failed: %w", err)
	}
	if st, err := os.Lstat(tempPath); err != nil || !st.Mode().IsRegular() || fileLinkCount(st) != 1 {
		return "", fmt.Errorf("owned temporary file identity broken before publication")
	}
	if producePublishHook != nil {
		producePublishHook(tempPath, absOut)
	}
	// Revalidate the pinned parent AND owned-root identities at
	// publication: a root or parent swapped between capture and publish is
	// deterministically refused instead of receiving manifest authority.
	parentNow, err := os.Lstat(realParent)
	if err != nil || !os.SameFile(parentInfo, parentNow) {
		return "", fmt.Errorf("output parent replaced between identity capture and publication")
	}
	rootNowPub, err := os.Lstat(rootDir)
	if err != nil {
		return "", fmt.Errorf("owned root identity at publication: %w", err)
	}
	if !os.SameFile(rootHandleInfo, rootNowPub) {
		return "", fmt.Errorf("owned root replaced between identity capture and publication")
	}
	finalPath := filepath.Join(realParent, filepath.Base(absOut))
	// No-replace atomic publication: os.Link materializes the fully written
	// inode at the destination and fails when any destination already
	// exists — a reader observes either the absent destination or the
	// complete manifest, never a partial file, and an existing artifact is
	// never clobbered.
	if err := os.Link(tempPath, finalPath); err != nil {
		if _, statErr := os.Lstat(finalPath); statErr == nil {
			return "", fmt.Errorf("output %s already exists; the manifest is never overwritten", out)
		}
		return "", fmt.Errorf("atomic publication refused: %w", err)
	}
	// Establish durable directory-entry publication where the platform
	// supports it: the failure to open or sync the containing directory is
	// a publication failure, not telemetry — the manifest inode exists at
	// the destination but a crash could lose the directory entry. The
	// published final file is preserved (it is the reviewed publication,
	// never deleted or replaced), the owned temporary is cleaned by the
	// deferred cleanup, and the error describes the partial publication.
	if err := produceSyncDirectory(realParent); err != nil {
		return "", fmt.Errorf("manifest %s is published at the destination but directory durability could not be established (partial publication): %w", finalPath, err)
	}
	published, err := os.Lstat(finalPath)
	if err != nil || !published.Mode().IsRegular() || !os.SameFile(tempInfo, published) {
		return "", fmt.Errorf("published output is not the completely written manifest inode")
	}
	return finalPath, nil
}
