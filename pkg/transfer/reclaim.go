// Package transfer implements evidence-backed reclamation of git transfer
// bundles produced and consumed by coordinator review flows.
//
// Retention contract: a bundle is reclaimable only when every gate holds —
//
//   - native serialization: the pass runs under the shared-checkout DirLock,
//     serializing reclaimers against each other and against participating
//     transfers that hold the same native lock;
//   - manifest authority: the coordinator's retention manifest inside the
//     owned root lists the exact bundle filename; repository location and a
//     `.bundle` suffix never prove ownership;
//   - canonical identity: the bundle directory resolves to the canonical
//     repository (itself or one of its worktrees) and lives inside owned
//     `.herd` state, never arbitrary user paths;
//   - regular file: Lstat shows a regular file, never a symlink, with a
//     link count of exactly one;
//   - integrity: `git bundle verify` passes against the canonical repo;
//   - containment: every contained tip is retained by canonical refs
//     (present locally AND reachable from `--all`), with list-heads output
//     parsed strictly — malformed or partial output is an unknown;
//   - no readers: authoritative lsof proof that no process holds it open;
//   - revalidation: parent-directory identity, file identity, reader proof,
//     and containment are rechecked immediately before a directory-relative
//     unlink pinned to the parent directory descriptor.
//
// Any error, unknown reader, changed file or parent, missing or unique
// object, hard-linked inode, timed-out or overflowing child, or malformed
// output retains the bundle with an actionable reason. Non-`.bundle`
// entries (receipt logs, matrices, recovery records) are never candidates.
// A dry-run never unlinks and never reports bytes as freed. Directory
// iteration and every child process lifetime/output are bounded.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/lock"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const (
	bundleSuffix = ".bundle"

	DefaultMaxFiles        = 256
	DefaultMaxReclaimBytes = int64(4) << 30

	// dirChunkSize bounds directory iteration: entries are streamed in
	// chunks and enumeration stops once the bundle-name budget is met, so a
	// directory with unbounded non-bundle evidence is never fully loaded.
	dirChunkSize = 256

	// reclaimMaxPassDuration is the longest pass the CLI schedules (the
	// 15-minute bundle-reclaim context). The native lock's stale-age bound
	// must exceed any supported pass so a LIVE holder is never broken for
	// age mid-pass; dead holders are still broken immediately by pid.
	reclaimMaxPassDuration  = 15 * time.Minute
	bundleReclaimLockMaxAge = 35 * time.Minute
)

// ReaderStatus is deliberately tri-state. Missing reader inspection never
// proves absence, and absence must be proven, not assumed.
type ReaderStatus string

const (
	ReaderAbsent  ReaderStatus = "absent"
	ReaderPresent ReaderStatus = "present"
	ReaderUnknown ReaderStatus = "unknown"
)

// ReclaimOptions bound and scope one reclamation pass.
type ReclaimOptions struct {
	// RepoRoot must resolve to the canonical repository checkout.
	RepoRoot string
	// Root is the owned bundle directory under `.herd` state.
	Root string
	// Manifest is the required coordinator-owned retention manifest inside
	// Root listing the exact bundle filenames that may be considered.
	Manifest string
	// Act performs unlinks. Zero value is a dry-run that only reports.
	Act bool
	// LockDir is the native shared-checkout lock directory serializing
	// reclaimers and participating transfers. Empty defaults to the
	// canonical repo's `.git/herd-shared-checkout.lock.d`.
	LockDir string
	// LockWait bounds how long the pass waits for the native lock.
	LockWait time.Duration
	// MaxFiles bounds how many bundle files one pass examines.
	MaxFiles int
	// MaxBytes bounds reclaimed (act) or would-reclaim (dry-run) bytes.
	MaxBytes int64
	// MinAge retains files modified more recently than this window
	// (active-transfer guard). Zero disables the window.
	MinAge time.Duration
	// Protect retains exactly-named bundle files regardless of eligibility.
	Protect []string
	// LiveReader overrides the default lsof reader proof.
	LiveReader func(ctx context.Context, path string) (ReaderStatus, error)
}

// FileDisposition records one scanned bundle file's outcome. Digest is the
// manifest-pinned content identity admitted for deletion, carried into the
// receipt so every reclaimed name is bound to the exact bytes deleted.
type FileDisposition struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
	Digest string `json:"digest,omitempty"`
}

// ReclaimReport is the per-pass receipt. ReclaimedBytes is actual freed
// space in act mode only; a dry-run reports WouldReclaimBytes and keeps
// ReclaimedBytes at zero.
type ReclaimReport struct {
	DryRun            bool              `json:"dry_run"`
	Scanned           int               `json:"scanned"`
	NonBundleEntries  int               `json:"non_bundle_entries"`
	Candidates        int               `json:"candidates"`
	Reclaimed         int               `json:"reclaimed"`
	ReclaimedBytes    int64             `json:"reclaimed_bytes"`
	WouldReclaimBytes int64             `json:"would_reclaim_bytes"`
	Partial           bool              `json:"partial"`
	Reason            string            `json:"reason,omitempty"`
	Dispositions      []FileDisposition `json:"dispositions"`
}

type bundleTip struct {
	SHA string
	Ref string
}

// Reclaim runs one bounded, evidence-backed pass over opts.Root.
func Reclaim(ctx context.Context, opts ReclaimOptions) (ReclaimReport, error) {
	report := ReclaimReport{DryRun: !opts.Act}
	dir, err := resolveScope(ctx, opts)
	if err != nil {
		return report, err
	}
	if strings.TrimSpace(opts.LockDir) == "" {
		opts.LockDir = filepath.Join(opts.RepoRoot, lock.DefaultRelDir)
	}
	lockDirAbs, err := filepath.EvalSymlinks(filepath.Dir(opts.LockDir))
	if err != nil {
		return report, fmt.Errorf("bundle reclaim: native lock parent: %w", err)
	}
	opts.LockDir = filepath.Join(lockDirAbs, filepath.Base(opts.LockDir))
	shared := lock.NewDirLock(opts.LockDir)
	// A reclaim pass may legitimately run for the whole 15-minute CLI window;
	// the lock's stale-age bound must never fire while this holder is alive.
	shared.SetMaxAge(bundleReclaimLockMaxAge)
	var lockOwned bool
	// Re-entrancy is exclusively the `herd lock with` contract: the env names
	// THIS lockdir. An existing lock held by anyone else must block/refuse
	// through Acquire, never be adopted — and never released by this pass.
	if os.Getenv(lock.EnvHeld) != opts.LockDir {
		wait := opts.LockWait
		if wait <= 0 {
			wait = 30 * time.Second
		}
		if err := shared.Acquire(ctx, wait, "herd bundle-reclaim serializes reclaimers and participating transfers"); err != nil {
			return report, fmt.Errorf("bundle reclaim: native lock: %w", err)
		}
		lockOwned = true
	}
	if lockOwned {
		defer shared.Release()
	}

	// The authority snapshot is taken under the native lock, never before
	// blocking on it, and is revalidated against the file immediately
	// before every unlink.
	manifest, err := LoadRetentionManifest(dir, opts.Manifest)
	if err != nil {
		return report, err
	}

	maxFiles := opts.MaxFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxFiles
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxReclaimBytes
	}
	protected := make(map[string]bool, len(opts.Protect))
	for _, name := range opts.Protect {
		protected[name] = true
	}
	reader := opts.LiveReader
	if reader == nil {
		reader = DefaultLsofReader
	}
	// plannedBytes is the ONE cumulative byte counter the budget enforces:
	// it advances with would-reclaim bytes in dry-run and with actually
	// reclaimed bytes in act, so --max-bytes bounds destructive work.
	var plannedBytes int64

	// Bounded, deterministic enumeration: stream directory chunks, collect
	// only manifest-relevant .bundle names up to the file budget, then sort.
	// Enumeration is fail-closed: a directory-read error fails the whole
	// pass instead of truncating the census silently.
	dirFile, err := os.Open(dir)
	if err != nil {
		return report, fmt.Errorf("bundle reclaim: read %s: %w", dir, err)
	}
	defer dirFile.Close()
	bundleNames, enumErr := enumerateBundleNames(dirFile, dirChunkSize, maxFiles, ctx, &report)
	if enumErr != nil {
		return report, fmt.Errorf("bundle reclaim: enumeration failed fail-closed: %w", enumErr)
	}
	if report.Partial && report.Reason == "timeout" {
		return report, nil
	}
	sort.Strings(bundleNames)

	for _, name := range bundleNames {
		report.Scanned++
		disp := FileDisposition{Name: name}
		path := filepath.Join(dir, name)
		st, statErr := os.Lstat(path)
		if statErr != nil {
			disp.Action, disp.Reason = "retained", fmt.Sprintf("stat-unknown: %v", statErr)
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		disp.Bytes = st.Size()
		entry, admitted := manifestEntry(manifest, name)
		if !admitted {
			disp.Action, disp.Reason = "retained", "not-in-retention-manifest"
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "content-identity-mismatch",
			st.Size() != entry.Size || st.ModTime().UnixNano() != entry.ModTimeUnixNano, ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		disp.Digest = entry.Digest
		if retain := gate(&disp, "not-regular-file", !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0, ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "explicitly-protected", protected[name], ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "hard-link-substitution-possible", fileLinkCount(st) != 1, ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if opts.MinAge > 0 && time.Since(st.ModTime()) < opts.MinAge {
			disp.Action, disp.Reason = "retained", "recently-modified-active-transfer-window"
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "bundle-verify-failed", !verifyBundle(ctx, opts.RepoRoot, path), ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		tips, tipsErr := listBundleTips(ctx, path)
		if tipsErr != nil || len(tips) == 0 {
			disp.Action, disp.Reason = "retained", fmt.Sprintf("bundle-tips-unknown: %v", tipsErr)
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if reason := containmentReason(ctx, opts.RepoRoot, tips); reason != "" {
			disp.Action, disp.Reason = "retained", reason
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		status, readerErr := reader(ctx, path)
		if readerErr != nil {
			disp.Action, disp.Reason = "retained", fmt.Sprintf("reader-proof-unknown: %v", readerErr)
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "reader-present", status == ReaderPresent, ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "reader-unknown", status == ReaderUnknown, ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if plannedBytes+st.Size() > maxBytes {
			report.Partial, report.Reason = true, "byte-budget"
			break
		}
		if !opts.Act {
			report.Candidates++
			report.WouldReclaimBytes += st.Size()
			plannedBytes += st.Size()
			disp.Action = "eligible-candidate"
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		dirBefore, dirBeforeErr := os.Lstat(dir)
		if dirBeforeErr != nil {
			disp.Action, disp.Reason = "retained", fmt.Sprintf("parent-stat-unknown: %v", dirBeforeErr)
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if reason := revalidateBeforeUnlink(ctx, opts, reader, dir, dirFile, dirBefore, path, st, tips, entry.Digest); reason != "" {
			disp.Action, disp.Reason = "retained", reason
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if reason := revalidateManifestEntry(opts.Manifest, dir, name, entry); reason != "" {
			disp.Action, disp.Reason = "retained", reason
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		report.Candidates++
		if interleaveHook != nil {
			interleaveHook("reclaim-pre-unlink")
		}
		// removeReviewedEntry is a package variable so interleaving tests can
		// observe this exact boundary; production uses the
		// rename-into-quarantine, verify-then-unlink primitive that proves
		// the moved object's identity before destruction.
		reclaimed, deleteReason := removeReviewedEntry(dirFile, dir, name, st, entry.Digest)
		if !reclaimed {
			disp.Action, disp.Reason = "retained", deleteReason
			report.Dispositions = append(report.Dispositions, disp)
			report.Candidates--
			continue
		}
		if _, readbackErr := os.Lstat(path); !os.IsNotExist(readbackErr) {
			// The reviewed object was deleted from its quarantine path, but
			// the origin name resolves again: a writer installed a
			// replacement in the residual window. The replacement is
			// preserved and the reclaim is reported truthfully.
			disp.Action, disp.Reason = "reclaimed", "name-replaced-after-quarantine-unlink: replacement preserved"
			report.Dispositions = append(report.Dispositions, disp)
			report.Reclaimed++
			report.ReclaimedBytes += st.Size()
			plannedBytes += st.Size()
			continue
		}
		report.Reclaimed++
		report.ReclaimedBytes += st.Size()
		plannedBytes += st.Size()
		disp.Action = "reclaimed"
		report.Dispositions = append(report.Dispositions, disp)
	}
	return report, nil
}

// revalidateManifestEntry re-reads the coordinator's manifest under the
// native lock immediately before unlink and requires the same pinned
// content identity. A withdrawn entry, a rewritten manifest, or any load
// failure revokes deletion authority.
func revalidateManifestEntry(manifestPath, root, name string, admitted RetentionEntry) string {
	current, err := LoadRetentionManifest(root, manifestPath)
	if err != nil {
		return fmt.Sprintf("manifest-withdrawn-under-lock: %v", err)
	}
	entry, ok := manifestEntry(current, name)
	if !ok {
		return "manifest-withdrawn-under-lock: bundle no longer listed by the coordinator manifest"
	}
	if entry != admitted {
		return "manifest-withdrawn-under-lock: admitted identity no longer matches the manifest"
	}
	return ""
}

// enumerateBundleNames streams directory chunks until EOF, budget, or
// timeout. Any other read error is a hard failure — a truncated census
// must never look like a complete one.
func enumerateBundleNames(d dirReader, chunk, maxFiles int, ctx context.Context, report *ReclaimReport) ([]string, error) {
	var names []string
	for {
		if ctx.Err() != nil {
			report.Partial, report.Reason = true, "timeout"
			return names, nil
		}
		entries, readErr := d.ReadDir(chunk)
		for _, entry := range entries {
			name := entry.Name()
			if !strings.HasSuffix(name, bundleSuffix) {
				report.NonBundleEntries++
				continue
			}
			if len(names) >= maxFiles {
				report.Partial, report.Reason = true, "file-budget"
				return names, nil
			}
			names = append(names, name)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return names, nil
			}
			return nil, readErr
		}
	}
}

// dirReader is the slice of *os.File enumeration consumes.
type dirReader interface {
	ReadDir(n int) ([]os.DirEntry, error)
}

func gate(disp *FileDisposition, reason string, cond bool, detail string) bool {
	if !cond {
		return false
	}
	disp.Action = "retained"
	disp.Reason = reason
	if detail != "" {
		disp.Reason = reason + ": " + detail
	}
	return true
}

// resolveScope enforces canonical repository identity and the owned-root
// boundary before any bundle is examined.
func resolveScope(ctx context.Context, opts ReclaimOptions) (string, error) {
	if strings.TrimSpace(opts.RepoRoot) == "" || strings.TrimSpace(opts.Root) == "" {
		return "", fmt.Errorf("bundle reclaim: canonical repo root and bundle directory are required")
	}
	canon, err := worktree.ResolveCanonicalRoot(ctx, opts.RepoRoot, "")
	if err != nil {
		return "", fmt.Errorf("bundle reclaim: canonical repository identity: %w", err)
	}
	dir, err := filepath.EvalSymlinks(opts.Root)
	if err != nil {
		return "", fmt.Errorf("bundle reclaim: bundle directory: %w", err)
	}
	if fi, err := os.Lstat(opts.Root); err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("bundle reclaim: bundle directory must be a real directory")
	}
	top, err := gitroot.Toplevel(ctx, dir)
	if err != nil {
		return "", fmt.Errorf("bundle reclaim: bundle directory is not inside the repository: %w", err)
	}
	dirCanon, err := worktree.ResolveCanonicalRoot(ctx, strings.TrimSpace(top), "")
	if err != nil {
		return "", fmt.Errorf("bundle reclaim: bundle directory repository identity: %w", err)
	}
	canonAbs, err := filepath.EvalSymlinks(canon)
	if err != nil {
		return "", fmt.Errorf("bundle reclaim: canonical repository identity: %w", err)
	}
	if filepath.Clean(dirCanon) != filepath.Clean(canonAbs) {
		return "", fmt.Errorf("bundle reclaim: bundle directory belongs to another repository")
	}
	// The bundle directory must live inside the containing worktree's OWN
	// .herd state — any `.herd` component anywhere above (pool slots, outer
	// checkouts) does not confer ownership. top is the actual repository
	// that contains dir; the component-safe relative path from top must
	// enter .herd first.
	topReal, err := filepath.EvalSymlinks(strings.TrimSpace(top))
	if err != nil {
		return "", fmt.Errorf("bundle reclaim: bundle directory repository root: %w", err)
	}
	rel, err := filepath.Rel(topReal, dir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, "../") ||
		(rel != ".herd" && !strings.HasPrefix(rel, ".herd/")) {
		return "", fmt.Errorf("bundle reclaim: bundle directory is not inside the containing repository's owned .herd state")
	}
	return dir, nil
}

func verifyBundle(ctx context.Context, repoRoot, path string) bool {
	_, err := runBounded(ctx, defaultGitTimeout, gitOutputCapBytes, repoRoot, "git", "-C", repoRoot, "bundle", "verify", path)
	return err == nil
}

// parseListHeadsOutput parses `git bundle list-heads` output strictly: every
// non-empty line must be exactly one 40-hex sha and one non-empty ref.
// Malformed or partial output is an error — a skipped line would silently
// skip a tip check and cannot prove all tips were verified.
func parseListHeadsOutput(data string) ([]bundleTip, error) {
	var tips []bundleTip
	for _, line := range strings.Split(data, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("malformed list-heads line %q", line)
		}
		sha, ref := fields[0], fields[1]
		if len(sha) != 40 {
			return nil, fmt.Errorf("malformed list-heads sha %q", sha)
		}
		for _, r := range sha {
			if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
				return nil, fmt.Errorf("malformed list-heads sha %q", sha)
			}
		}
		if strings.TrimSpace(ref) == "" {
			return nil, fmt.Errorf("malformed list-heads ref in line %q", line)
		}
		tips = append(tips, bundleTip{SHA: sha, Ref: ref})
	}
	if len(tips) == 0 {
		return nil, fmt.Errorf("list-heads output contained no refs")
	}
	return tips, nil
}

func listBundleTips(ctx context.Context, path string) ([]bundleTip, error) {
	out, err := runBounded(ctx, defaultGitTimeout, gitOutputCapBytes, "", "git", "bundle", "list-heads", path)
	if err != nil {
		return nil, err
	}
	return parseListHeadsOutput(out)
}

// containmentReason returns "" when every tip is retained by canonical refs;
// otherwise it returns an actionable retention reason.
func containmentReason(ctx context.Context, repoRoot string, tips []bundleTip) string {
	cctx, cancel := context.WithTimeout(ctx, defaultGitTimeout)
	defer cancel()
	cmd := gitCommand(cctx, repoRoot, "git", "cat-file", "--batch-check", "--buffer")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Sprintf("tip-existence-unknown: %v", err)
	}
	var stdout boundedBuffer
	stdout.limit = gitOutputCapBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	cmd.WaitDelay = childWaitDelay
	if err := cmd.Start(); err != nil {
		return fmt.Sprintf("tip-existence-unknown: %v", err)
	}
	for _, tip := range tips {
		if _, err := fmt.Fprintf(stdin, "%s^{commit}\n", tip.SHA); err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return fmt.Sprintf("tip-existence-unknown: %v", err)
		}
	}
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Sprintf("tip-existence-unknown: %v", err)
	}
	if stdout.overflow {
		return "tip-existence-unknown: batch-check output exceeded bound"
	}
	for _, line := range strings.Split(stdout.String(), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[1] == "missing" {
			for _, tip := range tips {
				if strings.EqualFold(tip.SHA, fields[0]) {
					return fmt.Sprintf("tip-missing-from-canonical: %s (%s) is not retained by any canonical ref", tip.Ref, tip.SHA)
				}
			}
			return fmt.Sprintf("tip-missing-from-canonical: %s is not retained by any canonical ref", fields[0])
		}
	}
	args := []string{"git", "-C", repoRoot, "rev-list", "--count"}
	for _, tip := range tips {
		args = append(args, tip.SHA)
	}
	args = append(args, "--not", "--all")
	out, err := runBounded(ctx, defaultGitTimeout, gitOutputCapBytes, repoRoot, args...)
	if err != nil {
		return fmt.Sprintf("reachability-unknown: %v", err)
	}
	if strings.TrimSpace(out) != "0" {
		return "tips-not-retained-by-canonical-refs: canonical refs no longer contain every bundle tip"
	}
	return ""
}

// revalidateBeforeUnlink re-runs scope, parent identity, reader, and
// containment proof immediately before the directory-relative unlink; any
// drift retains the bundle.
func revalidateBeforeUnlink(ctx context.Context, opts ReclaimOptions, reader func(context.Context, string) (ReaderStatus, error), dir string, dirFile *os.File, dirBefore os.FileInfo, path string, before os.FileInfo, tips []bundleTip, pinnedDigest string) string {
	dirNow, err := os.Lstat(dir)
	if err != nil || dirBefore == nil || !os.SameFile(dirBefore, dirNow) {
		return "parent-changed-during-reclaim: bundle directory identity moved between census and unlink"
	}
	if pinned, pinErr := dirFile.Stat(); pinErr != nil || !os.SameFile(pinned, dirNow) {
		return "parent-changed-during-reclaim: pinned directory descriptor no longer matches the bundle directory"
	}
	st, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, st) || st.Size() != before.Size() || !st.ModTime().Equal(before.ModTime()) {
		return "changed-during-reclaim: bundle identity moved between census and unlink"
	}
	if fileLinkCount(st) != 1 {
		return "hard-link-substitution-possible-at-unlink"
	}
	status, readerErr := reader(ctx, path)
	if readerErr != nil {
		return fmt.Sprintf("reader-proof-unknown: %v", readerErr)
	}
	if status != ReaderAbsent {
		return fmt.Sprintf("reader-%s-at-unlink", status)
	}
	digest, err := fileContentDigest(path, before.Size())
	if err != nil {
		return fmt.Sprintf("content-identity-unknown-at-unlink: %v", err)
	}
	if digest != pinnedDigest {
		return fmt.Sprintf("content-identity-mismatch-at-unlink: sha256 %s does not match the manifest-pinned digest", digest)
	}
	if !verifyBundle(ctx, opts.RepoRoot, path) {
		return "bundle-verify-failed-at-unlink"
	}
	if reason := containmentReason(ctx, opts.RepoRoot, tips); reason != "" {
		return "revalidated-" + reason
	}
	return ""
}

// fileContentDigest streams the file's full content through SHA-256 under a
// size bound: more than wantSize bytes is a mismatch, never a truncation.
func fileContentDigest(path string, wantSize int64) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, wantSize+1))
	if err != nil {
		return "", err
	}
	if n != wantSize {
		return "", fmt.Errorf("content length %d does not match pinned size %d", n, wantSize)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// DefaultLsofReader is the authoritative no-reader proof. Missing tooling,
// partial output, unexpected exits, timeouts, or overflow are unknown, never
// absence.
func DefaultLsofReader(ctx context.Context, path string) (ReaderStatus, error) {
	lsof, err := execLookPath("lsof")
	if err != nil {
		return ReaderUnknown, fmt.Errorf("reader proof unavailable: %w", err)
	}
	out, runErr := runBounded(ctx, defaultLsofTimeout, lsofOutputCapBytes, "", lsof, "--", path)
	if runErr != nil {
		// lsof's canonical "nothing open" answer is exit 1 with no output.
		// Only that exact shape proves absence; timeouts and overflow stay
		// unknown.
		var exitErr *exec.ExitError
		if !strings.Contains(runErr.Error(), "timed out") && !strings.Contains(runErr.Error(), "exceeded") &&
			errors.As(runErr, &exitErr) && exitErr.ExitCode() == 1 && len(strings.TrimSpace(out)) == 0 {
			return ReaderAbsent, nil
		}
		return ReaderUnknown, fmt.Errorf("lsof: %w", runErr)
	}
	if len(strings.TrimSpace(out)) > 0 {
		return ReaderPresent, nil
	}
	return ReaderUnknown, fmt.Errorf("lsof returned empty success")
}

// removeReviewedEntry is the deletion primitive invoked after every
// revalidation has passed. It is a package variable so interleaving tests
// can observe the exact proof/deletion boundary; production binds it to
// quarantineAndRemove, which proves the moved object's identity before
// destroying it and restores any foreign replacement.
var removeReviewedEntry = quarantineAndRemove
