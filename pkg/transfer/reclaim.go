// Package transfer implements evidence-backed reclamation of git transfer
// bundles produced and consumed by coordinator review flows.
//
// Retention contract: a bundle is reclaimable only when every gate holds —
//
//   - canonical identity: the bundle directory resolves to the canonical
//     repository (itself or one of its worktrees) and lives inside owned
//     `.herd` state, never arbitrary user paths;
//   - regular file: Lstat shows a regular file, never a symlink;
//   - integrity: `git bundle verify` passes against the canonical repo;
//   - containment: every contained tip is retained by canonical refs
//     (present locally AND reachable from `--all`);
//   - no readers: authoritative lsof proof that no process holds it open;
//   - revalidation: identity, reader proof, and containment are rechecked
//     immediately before unlink.
//
// Any error, unknown reader, changed file, or missing or unique object
// retains the bundle with an actionable reason. Non-`.bundle` entries
// (receipt logs, matrices, recovery records) are never candidates.
// A dry-run never unlinks and never reports bytes as freed.
package transfer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/worktree"
)

const (
	bundleSuffix = ".bundle"

	DefaultMaxFiles        = 256
	DefaultMaxReclaimBytes = int64(4) << 30
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
	// Act performs unlinks. Zero value is a dry-run that only reports.
	Act bool
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

// FileDisposition records one scanned bundle file's outcome.
type FileDisposition struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	Action string `json:"action"`
	Reason string `json:"reason,omitempty"`
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

	entries, err := os.ReadDir(dir)
	if err != nil {
		return report, fmt.Errorf("bundle reclaim: read %s: %w", dir, err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if ctx.Err() != nil {
			report.Partial, report.Reason = true, "timeout"
			break
		}
		name := entry.Name()
		if !strings.HasSuffix(name, bundleSuffix) {
			report.NonBundleEntries++
			continue
		}
		if report.Scanned >= maxFiles {
			report.Partial, report.Reason = true, "file-budget"
			break
		}
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
		if retain := gate(&disp, "not-regular-file", !st.Mode().IsRegular() || st.Mode()&os.ModeSymlink != 0, ""); retain {
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if retain := gate(&disp, "explicitly-protected", protected[name], ""); retain {
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
		if report.WouldReclaimBytes+st.Size() > maxBytes {
			report.Partial, report.Reason = true, "byte-budget"
			break
		}
		if !opts.Act {
			report.Candidates++
			report.WouldReclaimBytes += st.Size()
			disp.Action = "eligible-candidate"
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if reason := revalidateBeforeUnlink(ctx, opts, reader, path, st, tips); reason != "" {
			disp.Action, disp.Reason = "retained", reason
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		report.Candidates++
		if err := os.Remove(path); err != nil {
			disp.Action, disp.Reason = "retained", fmt.Sprintf("unlink-failed: %v", err)
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		if _, readbackErr := os.Lstat(path); !os.IsNotExist(readbackErr) {
			disp.Action, disp.Reason = "retained", "unlink-readback-unknown"
			report.Dispositions = append(report.Dispositions, disp)
			continue
		}
		report.Reclaimed++
		report.ReclaimedBytes += st.Size()
		disp.Action = "reclaimed"
		report.Dispositions = append(report.Dispositions, disp)
	}
	return report, nil
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
	top, err := gitOutput(ctx, dir, "rev-parse", "--show-toplevel")
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
	if !hasPathComponent(dir, ".herd") {
		return "", fmt.Errorf("bundle reclaim: bundle directory is not inside owned .herd state")
	}
	return dir, nil
}

func hasPathComponent(path, want string) bool {
	for component := path; component != filepath.Dir(component); component = filepath.Dir(component) {
		if filepath.Base(component) == want {
			return true
		}
	}
	return false
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func verifyBundle(ctx context.Context, repoRoot, path string) bool {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "bundle", "verify", path)
	return cmd.Run() == nil
}

func listBundleTips(ctx context.Context, path string) ([]bundleTip, error) {
	out, err := exec.CommandContext(ctx, "git", "bundle", "list-heads", path).Output()
	if err != nil {
		return nil, err
	}
	var tips []bundleTip
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		tips = append(tips, bundleTip{SHA: fields[0], Ref: fields[1]})
	}
	if len(tips) == 0 {
		return nil, fmt.Errorf("bundle lists no refs")
	}
	return tips, nil
}

// containmentReason returns "" when every tip is retained by canonical refs;
// otherwise it returns an actionable retention reason.
func containmentReason(ctx context.Context, repoRoot string, tips []bundleTip) string {
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "cat-file", "--batch-check", "--buffer")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Sprintf("tip-existence-unknown: %v", err)
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
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
	args := []string{"-C", repoRoot, "rev-list", "--count"}
	for _, tip := range tips {
		args = append(args, tip.SHA)
	}
	args = append(args, "--not", "--all")
	out, err := exec.CommandContext(ctx, "git", args...).Output()
	if err != nil {
		return fmt.Sprintf("reachability-unknown: %v", err)
	}
	if strings.TrimSpace(string(out)) != "0" {
		return "tips-not-retained-by-canonical-refs: canonical refs no longer contain every bundle tip"
	}
	return ""
}

// revalidateBeforeUnlink re-runs identity, reader, and containment proof
// immediately before unlink; any drift retains the bundle.
func revalidateBeforeUnlink(ctx context.Context, opts ReclaimOptions, reader func(context.Context, string) (ReaderStatus, error), path string, before os.FileInfo, tips []bundleTip) string {
	st, err := os.Lstat(path)
	if err != nil || !os.SameFile(before, st) || st.Size() != before.Size() || !st.ModTime().Equal(before.ModTime()) {
		return "changed-during-reclaim: bundle identity moved between census and unlink"
	}
	status, readerErr := reader(ctx, path)
	if readerErr != nil {
		return fmt.Sprintf("reader-proof-unknown: %v", readerErr)
	}
	if status != ReaderAbsent {
		return fmt.Sprintf("reader-%s-at-unlink", status)
	}
	if !verifyBundle(ctx, opts.RepoRoot, path) {
		return "bundle-verify-failed-at-unlink"
	}
	if reason := containmentReason(ctx, opts.RepoRoot, tips); reason != "" {
		return "revalidated-" + reason
	}
	return ""
}

// DefaultLsofReader is the authoritative no-reader proof. Missing tooling,
// partial output, or unexpected exits are unknown, never absence.
func DefaultLsofReader(ctx context.Context, path string) (ReaderStatus, error) {
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return ReaderUnknown, fmt.Errorf("reader proof unavailable: %w", err)
	}
	cmd := exec.CommandContext(ctx, lsof, "--", path)
	out, err := cmd.CombinedOutput()
	if err == nil && len(bytes.TrimSpace(out)) > 0 {
		return ReaderPresent, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && len(bytes.TrimSpace(out)) == 0 {
		return ReaderAbsent, nil
	}
	if err == nil && len(bytes.TrimSpace(out)) == 0 {
		return ReaderUnknown, fmt.Errorf("lsof returned empty success")
	}
	return ReaderUnknown, fmt.Errorf("lsof: %w", err)
}
