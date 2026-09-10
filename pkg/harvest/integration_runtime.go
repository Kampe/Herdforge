package harvest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"github.com/Kampe/Herdforge/pkg/provenance"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// RuntimeBinding proves the executable selected by new native invocations.
// Existing processes keep their old inode; this is not a claim that they were
// restarted. Paths are relative to the project control root.
type RuntimeBinding struct {
	Revision   string `json:"revision"`
	Digest     string `json:"digest"`
	Executable string `json:"executable"`
}

const (
	runtimeRetentionManifestVersion   = 1
	runtimeRetentionManifestName      = "runtime-retention.json"
	runtimeRetentionJournalName       = "runtime-retention.jsonl"
	defaultRuntimeRetentionCandidates = 16
	defaultRuntimeRetentionBytes      = 1 << 30
	defaultRuntimeRetentionTimeout    = 30 * time.Second
)

// RuntimeOwnerStatus is deliberately tri-state. Missing process inspection is
// not proof of absence and therefore never permits removal.
type RuntimeOwnerStatus string

const (
	RuntimeOwnerAbsent  RuntimeOwnerStatus = "absent"
	RuntimeOwnerPresent RuntimeOwnerStatus = "present"
	RuntimeOwnerUnknown RuntimeOwnerStatus = "unknown"
)

// RuntimeRetentionOptions bounds one maintenance pass. LiveOwner is a test
// seam; production uses lsof and treats an unavailable or malformed result as
// unknown.
type RuntimeRetentionOptions struct {
	MaxCandidates     int
	MaxAllocatedBytes int64
	Timeout           time.Duration
	LiveOwner         func(context.Context, string) (RuntimeOwnerStatus, error)
}

type RuntimeRetentionBinding struct {
	RuntimeBinding
	Path string `json:"path"`
	// FileID is receipt data only: a device:inode snapshot recorded for audit
	// receipts. Retention decisions compare digests, never FileID.
	FileID          string `json:"file_id"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mod_time_unix_nano"`
}

type RuntimeRetentionManifest struct {
	Version  int                       `json:"version"`
	Current  RuntimeRetentionBinding   `json:"current"`
	Previous []RuntimeRetentionBinding `json:"previous"`
}

type RuntimeRetentionEvent struct {
	At             time.Time `json:"at"`
	Event          string    `json:"event"`
	Path           string    `json:"path,omitempty"`
	Reason         string    `json:"reason,omitempty"`
	LogicalBytes   int64     `json:"logical_bytes,omitempty"`
	AllocatedBytes int64     `json:"allocated_bytes,omitempty"`
	Readback       string    `json:"readback,omitempty"`
}

// RuntimeRetentionReport is the operator-visible result of one bounded pass.
type RuntimeRetentionReport struct {
	Scanned               int                     `json:"scanned"`
	Removed               int                     `json:"removed"`
	Held                  int                     `json:"held"`
	Protected             int                     `json:"protected"`
	Errors                int                     `json:"errors"`
	LogicalBytesRemoved   int64                   `json:"logical_bytes_removed"`
	AllocatedBytesRemoved int64                   `json:"allocated_bytes_removed"`
	Partial               bool                    `json:"partial"`
	Reason                string                  `json:"reason,omitempty"`
	Events                []RuntimeRetentionEvent `json:"events,omitempty"`
}

// HerdRuntimeInstaller installs an already-built, exact landed Herdforge
// executable. Building occurs in the owned source worktree, never the shared
// checkout. This does not implement a consumer application's deployment.
type HerdRuntimeInstaller struct {
	Root      string
	Source    string
	Revision  string
	retention *RuntimeRetentionOptions
}

var runtimeInstallCapability = runtimeInstallSupported

// RuntimeInstallSupported reports whether this platform can produce the file
// metadata (owner, link count, inode identity) that retention requires to
// operate fail-closed. Callers must refuse install before any build or
// mutation when it is false; unknown metadata is never treated as absent.
func RuntimeInstallSupported() bool { return runtimeInstallCapability() }

func (r HerdRuntimeInstaller) validate(ctx context.Context) error {
	if !fullIntegrationSHA(r.Revision) || r.Root == "" || r.Source == "" {
		return fmt.Errorf("runtime bind: explicit project/source and full landed revision required")
	}
	root, err := worktree.ResolveCanonicalRoot(ctx, r.Root, "")
	if err != nil {
		return err
	}
	want, err := filepath.EvalSymlinks(r.Root)
	if err != nil {
		return err
	}
	want, err = filepath.Abs(want)
	if err != nil || filepath.Clean(root) != filepath.Clean(want) {
		return fmt.Errorf("runtime bind: destination is not the canonical project root")
	}
	sourceCommon, err := worktree.GitCommonDir(ctx, r.Source)
	if err != nil {
		return err
	}
	common, err := worktree.GitCommonDir(ctx, root)
	if err != nil || sourceCommon != common {
		return fmt.Errorf("runtime bind: source belongs to another repository")
	}
	head, err := gitOutput(ctx, r.Source, "rev-parse", "HEAD")
	if err != nil || strings.TrimSpace(head) != r.Revision {
		return fmt.Errorf("runtime bind: source HEAD does not match landed revision")
	}
	dirty, err := gitOutput(ctx, r.Source, "status", "--porcelain", gitroot.StatusUntrackedNormal)
	if err != nil || strings.TrimSpace(dirty) != "" {
		return fmt.Errorf("runtime bind: source is dirty or unreadable")
	}
	if err := gitroot.RequireAncestorContext(ctx, root, r.Revision, "origin/main"); err != nil {
		return fmt.Errorf("runtime bind: revision is not proven on origin/main: %w", err)
	}
	return nil
}

func (r HerdRuntimeInstaller) inspect(path string) (*RuntimeBinding, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("runtime bind: selected executable is not a regular executable file")
	}
	info, err := provenance.ReadExecutable(path, r.Source)
	if err != nil {
		return nil, err
	}
	if !info.Comparable {
		return nil, fmt.Errorf("runtime bind: selected executable module differs from source")
	}
	if err := provenance.Validate(info, r.Revision); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, fmt.Errorf("runtime bind: executable changed during metadata inspection")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("runtime bind: executable changed during digest inspection")
	}
	return &RuntimeBinding{Revision: r.Revision, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Executable: provenance.NativeExecutableRel}, nil
}

// inspectPrior authenticates an already-installed ancestor against its own
// embedded revision. The current source is intentionally not used as the
// revision to validate here: preserving an older installed binary is the
// input to the next install, not a claim that it is the new artifact.
func (r HerdRuntimeInstaller) inspectPrior(path string) (*RuntimeBinding, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("runtime bind: prior executable is not a regular executable file: %w", err)
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("runtime bind: prior executable is not a regular executable file")
	}
	info, err := provenance.ReadExecutable(path, r.Source)
	if err != nil {
		return nil, fmt.Errorf("runtime bind: prior executable identity is unknown: %w", err)
	}
	if !info.Comparable || !fullIntegrationSHA(info.BinaryRevision) {
		return nil, fmt.Errorf("runtime bind: prior executable identity is unknown")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(st, opened) {
		return nil, fmt.Errorf("runtime bind: prior executable changed during metadata inspection")
	}
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return nil, err
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(opened, after) || opened.Size() != after.Size() || !opened.ModTime().Equal(after.ModTime()) {
		return nil, fmt.Errorf("runtime bind: prior executable changed during digest inspection")
	}
	return &RuntimeBinding{Revision: info.BinaryRevision, Digest: "sha256:" + hex.EncodeToString(h.Sum(nil)), Executable: provenance.NativeExecutableRel}, nil
}

var nativeRuntimeAliases = []struct{ path, target string }{{"herd", provenance.NativeExecutableRel}, {"bin/herdforge", "herd"}}

func (r HerdRuntimeInstaller) aliases(create bool) error {
	for _, a := range nativeRuntimeAliases {
		path := filepath.Join(r.Root, filepath.FromSlash(a.path))
		target, err := os.Readlink(path)
		if os.IsNotExist(err) && create {
			err = os.Symlink(a.target, path)
			if err == nil {
				continue
			}
			target, err = os.Readlink(path)
		}
		if err != nil || target != a.target {
			return fmt.Errorf("runtime bind: consumer alias %s is absent or does not select the canonical executable", a.path)
		}
	}
	return nil
}

// ObserveInstallation distinguishes an unapplied install from unknown state.
// nil, nil is permitted only for a missing target or a proven ancestor binary,
// with aliases either absent or already selecting the canonical executable.
// Foreign, unreadable and newer binaries refuse; errors are never pending work.
func (r HerdRuntimeInstaller) ObserveInstallation(ctx context.Context) (*RuntimeBinding, error) {
	if err := r.validate(ctx); err != nil {
		return nil, err
	}
	aliasesComplete := true
	for _, a := range nativeRuntimeAliases {
		target, err := os.Readlink(filepath.Join(r.Root, filepath.FromSlash(a.path)))
		if os.IsNotExist(err) {
			aliasesComplete = false
			continue
		}
		if err != nil || target != a.target {
			return nil, fmt.Errorf("runtime bind: consumer alias %s has an unknown or foreign target", a.path)
		}
	}
	path := filepath.Join(r.Root, "bin", "herd")
	st, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0111 == 0 {
		return nil, fmt.Errorf("runtime bind: current target is not a regular executable")
	}
	prior, err := provenance.ReadExecutable(path, r.Source)
	if err != nil || !prior.Comparable || !fullIntegrationSHA(prior.BinaryRevision) {
		return nil, fmt.Errorf("runtime bind: current executable identity is unknown")
	}
	if err := gitroot.RequireAncestorContext(ctx, r.Root, prior.BinaryRevision, r.Revision); err != nil {
		return nil, fmt.Errorf("runtime bind: current executable is newer or unrelated: %w", err)
	}
	if prior.BinaryRevision != r.Revision || !aliasesComplete {
		return nil, nil
	}
	return r.inspect(path)
}

// Observe is read-only. Missing, stale, or unknown binaries never prove a bind.
func (r HerdRuntimeInstaller) Observe(ctx context.Context) (*RuntimeBinding, error) {
	if err := r.validate(ctx); err != nil {
		return nil, err
	}
	if err := r.aliases(false); err != nil {
		return nil, err
	}
	return r.inspect(filepath.Join(r.Root, "bin", "herd"))
}

// Install holds the native shared-checkout lock, validates a copied temporary
// artifact before rename, and retains the previous inode by its content digest.
// A retry after rename simply reads the exact installed binding; it does not
// overwrite a newer runtime. It does not move refs, close panes, or mark Done.
func (r HerdRuntimeInstaller) Install(ctx context.Context) (*RuntimeBinding, error) {
	if !runtimeInstallCapability() {
		return nil, fmt.Errorf("runtime bind: unsupported platform: runtime file metadata is unavailable, refusing install before any mutation")
	}
	if err := r.validate(ctx); err != nil {
		return nil, err
	}
	common, err := worktree.GitCommonDir(ctx, r.Root)
	if err != nil {
		return nil, err
	}
	lockDir := filepath.Join(common, SharedIntegrationLockName)
	// A caller already holding this exact native lock retains ownership.
	if os.Getenv(lock.EnvHeld) != lockDir {
		dl := lock.NewDirLock(lockDir)
		if err := dl.Acquire(ctx, 0, "bind exact integration runtime"); err != nil {
			return nil, err
		}
		defer dl.Release()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := r.validate(ctx); err != nil {
		return nil, err
	}
	target := filepath.Join(r.Root, "bin", "herd")
	if binding, err := r.Observe(ctx); err == nil {
		return binding, nil
	}
	source := filepath.Join(r.Source, "bin", "herd")
	if _, err := r.inspect(source); err != nil {
		return nil, fmt.Errorf("runtime bind: build artifact refused: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return nil, err
	}
	var prior *RuntimeBinding
	var priorPath string
	if st, err := os.Lstat(target); err == nil {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("runtime bind: refusing to replace a non-regular target")
		}
		prior, err = r.inspectPrior(target)
		if err != nil {
			return nil, fmt.Errorf("runtime bind: prior executable identity is unknown: %w", err)
		}
		if prior == nil {
			return nil, fmt.Errorf("runtime bind: prior executable identity is unknown")
		}
		// A concurrent newer install, or unrelated binary, must never be downgraded.
		if err := gitroot.RequireAncestorContext(ctx, r.Root, prior.Revision, r.Revision); err != nil {
			return nil, fmt.Errorf("runtime bind: refusing runtime downgrade or unrelated history")
		}
		priorPath, err = r.preserve(target)
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".integration-runtime-*")
	if err != nil {
		return nil, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	defer tmp.Close()
	src, err := os.Open(source)
	if err != nil {
		return nil, err
	}
	_, copyErr := io.Copy(tmp, src)
	closeErr := src.Close()
	if copyErr != nil {
		return nil, copyErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if err := tmp.Chmod(0755); err != nil {
		return nil, err
	}
	if err := tmp.Sync(); err != nil {
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}
	if _, err := r.inspect(name); err != nil {
		return nil, err
	}
	if err := r.aliases(true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Rename(name, target); err != nil {
		return nil, err
	}
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return nil, err
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil {
		return nil, syncErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	binding, err := r.Observe(ctx)
	if err != nil {
		return nil, err
	}
	if err := r.retireRuntimeBackups(ctx, binding, prior, priorPath); err != nil {
		// The installed binding is real and intentionally returned alongside the
		// maintenance failure; callers must not interpret this as rollback.
		return binding, err
	}
	return binding, nil
}

func (r HerdRuntimeInstaller) preserve(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	dir := filepath.Join(r.Root, ".herd", "runtime-previous")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	backup := filepath.Join(dir, hex.EncodeToString(h.Sum(nil)))
	if err := os.Link(path, backup); err != nil {
		if !os.IsExist(err) {
			return "", err
		}
		a, e1 := os.Stat(path)
		b, e2 := os.Stat(backup)
		if e1 != nil || e2 != nil || !os.SameFile(a, b) {
			return "", fmt.Errorf("runtime bind: existing backup does not preserve prior inode")
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return "", err
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return "", err
	}
	return backup, nil
}

func (r HerdRuntimeInstaller) retentionOptions() RuntimeRetentionOptions {
	if r.retention == nil {
		return RuntimeRetentionOptions{}
	}
	return *r.retention
}

func (r HerdRuntimeInstaller) retentionPaths() (string, string) {
	dir := filepath.Join(r.Root, ".herd")
	return filepath.Join(dir, runtimeRetentionManifestName), filepath.Join(dir, runtimeRetentionJournalName)
}

func retentionBinding(path string, binding *RuntimeBinding) (RuntimeRetentionBinding, error) {
	st, err := os.Lstat(path)
	if err != nil {
		return RuntimeRetentionBinding{}, err
	}
	meta, ok := runtimeFileMetadata(st)
	if !ok {
		return RuntimeRetentionBinding{}, fmt.Errorf("runtime retention: file metadata unavailable")
	}
	return RuntimeRetentionBinding{RuntimeBinding: *binding, Path: path, FileID: meta.ID, Size: st.Size(), ModTimeUnixNano: st.ModTime().UnixNano()}, nil
}

func runtimeRetentionRelativePath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(rel)
}

func runtimeRetentionSameIdentity(a, b RuntimeRetentionBinding) bool {
	return a.Revision == b.Revision && a.Digest == b.Digest
}

func (r HerdRuntimeInstaller) loadRetentionManifest(path string) (*RuntimeRetentionManifest, error) {
	body, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var manifest RuntimeRetentionManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		return nil, fmt.Errorf("runtime retention: invalid manifest: %w", err)
	}
	if manifest.Version != runtimeRetentionManifestVersion || len(manifest.Previous) > 2 {
		return nil, fmt.Errorf("runtime retention: unsupported manifest version or chain")
	}
	for _, binding := range append([]RuntimeRetentionBinding{manifest.Current}, manifest.Previous...) {
		if binding.Path == "" || filepath.IsAbs(binding.Path) || binding.Path == ".." || strings.HasPrefix(binding.Path, "../") {
			return nil, fmt.Errorf("runtime retention: manifest path is not repository-relative")
		}
	}
	return &manifest, nil
}

func writeRuntimeRetentionManifest(path string, manifest RuntimeRetentionManifest) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".runtime-retention-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(body, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func appendRuntimeRetentionEvent(path string, event RuntimeRetentionEvent) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	if err := enc.Encode(event); err != nil {
		return err
	}
	return f.Sync()
}

func (r HerdRuntimeInstaller) retireRuntimeBackups(ctx context.Context, current, prior *RuntimeBinding, priorPath string) error {
	options := r.retentionOptions()
	maxCandidates := options.MaxCandidates
	if maxCandidates <= 0 {
		maxCandidates = defaultRuntimeRetentionCandidates
	}
	maxBytes := options.MaxAllocatedBytes
	if maxBytes <= 0 {
		maxBytes = defaultRuntimeRetentionBytes
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultRuntimeRetentionTimeout
	}
	retentionCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	manifestPath, journalPath := r.retentionPaths()
	manifest, err := r.loadRetentionManifest(manifestPath)
	if err != nil {
		return fmt.Errorf("runtime bind: installed binding preserved; retention manifest unknown: %w", err)
	}
	currentEntry, err := retentionBinding(filepath.Join(r.Root, "bin", "herd"), current)
	if err != nil {
		return fmt.Errorf("runtime bind: installed binding preserved; retention current binding: %w", err)
	}
	currentEntry.Path = runtimeRetentionRelativePath(r.Root, filepath.Join(r.Root, "bin", "herd"))
	if currentEntry.Path == "" {
		return fmt.Errorf("runtime bind: installed binding preserved; retention current path escaped root")
	}
	previous := make([]RuntimeRetentionBinding, 0, 2)
	if prior != nil {
		path := priorPath
		if path == "" {
			path = filepath.Join(r.Root, prior.Executable)
		}
		entry, entryErr := retentionBinding(path, prior)
		if entryErr != nil {
			return fmt.Errorf("runtime bind: installed binding preserved; retention prior binding: %w", entryErr)
		}
		entry.Path = runtimeRetentionRelativePath(r.Root, path)
		if entry.Path == "" {
			return fmt.Errorf("runtime bind: installed binding preserved; retention prior path escaped root")
		}
		previous = append(previous, entry)
	}
	if manifest != nil {
		if prior == nil || !runtimeRetentionSameIdentity(manifest.Current, previous[0]) {
			return fmt.Errorf("runtime bind: installed binding preserved; retention prior chain is ambiguous")
		}
		for _, old := range append([]RuntimeRetentionBinding(nil), manifest.Previous...) {
			duplicate := false
			for _, kept := range previous {
				if runtimeRetentionSameIdentity(kept, old) {
					duplicate = true
					break
				}
			}
			if !duplicate && len(previous) < 2 {
				previous = append(previous, old)
			}
		}
	}
	newManifest := RuntimeRetentionManifest{Version: runtimeRetentionManifestVersion, Current: currentEntry, Previous: previous}
	if err := writeRuntimeRetentionManifest(manifestPath, newManifest); err != nil {
		return fmt.Errorf("runtime bind: installed binding preserved; retention manifest write: %w", err)
	}
	report, err := r.retireRuntimeBackupsWith(retentionCtx, newManifest, journalPath, maxCandidates, maxBytes, options.LiveOwner)
	if err != nil {
		return fmt.Errorf("runtime bind: installed binding preserved; retention maintenance failed: %w", err)
	}
	if report.Partial || report.Held > 0 || report.Errors > 0 {
		if report.Reason == "" {
			report.Reason = "held-unknown-candidate"
		}
		return fmt.Errorf("runtime bind: installed binding preserved; retention maintenance partial: %s", report.Reason)
	}
	return nil
}

func (r HerdRuntimeInstaller) retireRuntimeBackupsWith(ctx context.Context, manifest RuntimeRetentionManifest, journalPath string, maxCandidates int, maxBytes int64, owner func(context.Context, string) (RuntimeOwnerStatus, error)) (RuntimeRetentionReport, error) {
	var report RuntimeRetentionReport
	dir := filepath.Join(r.Root, ".herd", "runtime-previous")
	st, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return report, nil
	}
	if err != nil {
		return report, err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return report, fmt.Errorf("runtime retention: backup directory is not a real directory")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return report, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			report.Partial, report.Reason = true, "timeout"
			break
		}
		if report.Scanned >= maxCandidates {
			report.Partial, report.Reason = true, "candidate-budget"
			break
		}
		report.Scanned++
		path := filepath.Join(dir, entry.Name())
		event := RuntimeRetentionEvent{At: time.Now().UTC(), Event: "inspect", Path: entry.Name()}
		if err := appendRuntimeRetentionEvent(journalPath, event); err != nil {
			return report, err
		}
		hold := func(reason string, protected bool) {
			if protected {
				report.Protected++
			} else {
				report.Held++
			}
			report.Events = append(report.Events, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "held", Path: entry.Name(), Reason: reason})
			if err := appendRuntimeRetentionEvent(journalPath, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "held", Path: entry.Name(), Reason: reason}); err != nil {
				report.Errors++
				report.Reason = "journal-write-failed"
			}
		}
		info, statErr := os.Lstat(path)
		if statErr != nil {
			hold("stat-unknown", false)
			continue
		}
		meta, ok := runtimeFileMetadata(info)
		uid, uidOK := runtimeCurrentUID()
		if !ok || !uidOK || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || meta.Links != 1 || meta.Owner != uid {
			hold("ownership-type-link-unknown", false)
			continue
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			hold("containment-unknown", false)
			continue
		}
		digest, digestErr := runtimeFileDigest(path)
		if digestErr != nil {
			hold("digest-unknown", false)
			continue
		}
		protected := false
		for _, binding := range manifest.Previous {
			if binding.Digest == digest {
				protected = true
				break
			}
		}
		if protected {
			hold("protected-receipt-bound-version", true)
			continue
		}
		ownerStatus, ownerErr := runtimeOwnerStatus(ctx, path, owner)
		if ownerErr != nil || ownerStatus != RuntimeOwnerAbsent {
			hold("live-owner-unknown-or-present", false)
			continue
		}
		if meta.Blocks*512 > maxBytes-report.AllocatedBytesRemoved {
			report.Partial, report.Reason = true, "allocated-byte-budget"
			break
		}
		// Recheck the installed target before every mutation. A changed current
		// binding makes cleanup unsafe; it never falls back to a filename.
		installed, installedErr := r.inspect(filepath.Join(r.Root, "bin", "herd"))
		if installedErr != nil || installed.Revision != manifest.Current.Revision || installed.Digest != manifest.Current.Digest {
			report.Errors++
			report.Reason = "current-install-changed"
			break
		}
		before, beforeErr := os.Lstat(path)
		if beforeErr != nil || !os.SameFile(info, before) || before.Size() != info.Size() || before.ModTime() != info.ModTime() {
			hold("candidate-changed", false)
			continue
		}
		logical, allocated := info.Size(), meta.Blocks*512
		if err := appendRuntimeRetentionEvent(journalPath, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "remove-intent", Path: entry.Name(), LogicalBytes: logical, AllocatedBytes: allocated}); err != nil {
			return report, err
		}
		if err := os.Remove(path); err != nil {
			report.Errors++
			report.Reason = "unlink-failed"
			_ = appendRuntimeRetentionEvent(journalPath, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "error", Path: entry.Name(), Reason: err.Error()})
			continue
		}
		_, readbackErr := os.Lstat(path)
		if !os.IsNotExist(readbackErr) {
			report.Errors++
			report.Reason = "unlink-readback-unknown"
			_ = appendRuntimeRetentionEvent(journalPath, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "error", Path: entry.Name(), Reason: "unlink-readback-unknown"})
			continue
		}
		report.Removed++
		report.LogicalBytesRemoved += logical
		report.AllocatedBytesRemoved += allocated
		report.Events = append(report.Events, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "removed", Path: entry.Name(), LogicalBytes: logical, AllocatedBytes: allocated, Readback: "absent"})
		if err := appendRuntimeRetentionEvent(journalPath, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "removed", Path: entry.Name(), LogicalBytes: logical, AllocatedBytes: allocated, Readback: "absent"}); err != nil {
			return report, err
		}
	}
	if err := appendRuntimeRetentionEvent(journalPath, RuntimeRetentionEvent{At: time.Now().UTC(), Event: "complete", Reason: report.Reason}); err != nil {
		return report, err
	}
	return report, nil
}

func runtimeOwnerStatus(ctx context.Context, path string, owner func(context.Context, string) (RuntimeOwnerStatus, error)) (RuntimeOwnerStatus, error) {
	if owner != nil {
		return owner(ctx, path)
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return RuntimeOwnerUnknown, err
	}
	cmd := exec.CommandContext(ctx, lsof, "-nP", "-Fpcfn", "--", path)
	out, err := cmd.CombinedOutput()
	if err == nil && len(out) > 0 {
		return RuntimeOwnerPresent, nil
	}
	if exitErr, ok := err.(*exec.ExitError); ok && exitErr.ExitCode() == 1 && len(out) == 0 {
		return RuntimeOwnerAbsent, nil
	}
	if err == nil && len(out) == 0 {
		return RuntimeOwnerUnknown, fmt.Errorf("lsof returned empty success")
	}
	return RuntimeOwnerUnknown, fmt.Errorf("lsof: %w", err)
}

func runtimeFileDigest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

func runtimeFileMetadata(info os.FileInfo) (runtimeFileMeta, bool) {
	return runtimeFileMetaFromInfo(info)
}
