package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// afterArchiveHook is test-only: it fires after an object is stored and
// before the source is re-hashed for drift.
var afterArchiveHook func(path string)

const (
	// ArtifactArchiveDir is the canonical, repository-relative archive root.
	ArtifactArchiveDir     = ".herd/artifact-archive"
	artifactArchiveVersion = 1
	kindReceipt            = "receipt"
	kindDerived            = "derived"
)

// ArtifactKind classifies an ignored path. Receipts must be archived before
// any removal. Derived binaries may be removed only after the same archive
// and readback.
type ArtifactKind string

const (
	ArtifactReceipt ArtifactKind = kindReceipt
	ArtifactDerived ArtifactKind = kindDerived
)

// ArtifactEntry is one archived ignored file.
type ArtifactEntry struct {
	Path   string       `json:"path"`
	Digest string       `json:"digest"`
	Size   int64        `json:"size"`
	Kind   ArtifactKind `json:"kind"`
}

// ArtifactManifest records every archived path for one target worktree.
type ArtifactManifest struct {
	Version int             `json:"version"`
	Target  string          `json:"target"`
	Entries []ArtifactEntry `json:"entries"`
}

// ArchiveRequest is a bounded archive-then-remove of ignored files in one
// registered worktree. Dry-run is the default.
type ArchiveRequest struct {
	Root    string
	Target  string
	Archive string
	Act     bool
	Now     time.Time
}

// ArchiveReport is the operator-visible outcome. Dry-run never sets Removed.
type ArchiveReport struct {
	Target   string          `json:"target"`
	Archive  string          `json:"archive"`
	Manifest string          `json:"manifest,omitempty"`
	Act      bool            `json:"act"`
	Entries  []ArtifactEntry `json:"entries"`
	Archived int             `json:"archived"`
	Removed  int             `json:"removed"`
	Reason   string          `json:"reason,omitempty"`
}

// ClassifyArtifactPath marks receipt evidence versus rebuildable derived files.
func ClassifyArtifactPath(rel string) ArtifactKind {
	p := filepath.ToSlash(rel)
	switch {
	case strings.Contains(p, "/.herd/receipts/") || strings.HasPrefix(p, ".herd/receipts/"):
		return ArtifactReceipt
	case strings.Contains(p, "/.herd/landed/") || strings.HasPrefix(p, ".herd/landed/"):
		return ArtifactReceipt
	case strings.Contains(p, "review-ledger") || strings.Contains(p, "harvest-queue") || strings.Contains(p, "harvestqueue"):
		return ArtifactReceipt
	default:
		return ArtifactDerived
	}
}

// ArchiveIgnored copies ignored files in target into a digest-addressed
// archive and, only on Act after readback, removes the sources. Receipts are
// archived before any unlink. Drift or an unexpired TASK-CONTEXT owner fails
// closed and leaves sources in place.
func ArchiveIgnored(req ArchiveRequest) (*ArchiveReport, error) {
	if req.Now.IsZero() {
		req.Now = time.Now()
	}
	root, err := absDir(req.Root)
	if err != nil {
		return nil, fmt.Errorf("artifact-archive: root: %w", err)
	}
	target := req.Target
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, target)
	}
	target, err = absDir(target)
	if err != nil {
		return nil, fmt.Errorf("artifact-archive: target: %w", err)
	}
	if sameArchivePath(root, target) {
		return nil, fmt.Errorf("artifact-archive: refusing the shared repository root")
	}
	relTarget, err := filepath.Rel(root, target)
	if err != nil || strings.HasPrefix(relTarget, "..") {
		return nil, fmt.Errorf("artifact-archive: target %s is outside the repository", target)
	}
	relTarget = filepath.ToSlash(relTarget)
	if err := requireRegisteredWorktree(root, target); err != nil {
		return nil, err
	}
	if err := refuseActiveOwner(target, req.Now); err != nil {
		return nil, err
	}

	archive := strings.TrimSpace(req.Archive)
	if archive == "" {
		archive = filepath.Join(root, filepath.FromSlash(ArtifactArchiveDir))
	} else if !filepath.IsAbs(archive) {
		archive = filepath.Join(root, archive)
	}
	archive, err = filepath.Abs(archive)
	if err != nil {
		return nil, fmt.Errorf("artifact-archive: archive: %w", err)
	}

	files, err := listIgnoredFiles(target)
	if err != nil {
		return nil, err
	}
	sort.SliceStable(files, func(i, j int) bool {
		ki, kj := ClassifyArtifactPath(files[i]), ClassifyArtifactPath(files[j])
		if ki != kj {
			return ki == ArtifactReceipt
		}
		return files[i] < files[j]
	})

	rep := &ArchiveReport{Target: relTarget, Archive: filepath.ToSlash(relPathOrAbs(root, archive)), Act: req.Act}
	if len(files) == 0 {
		rep.Reason = "no ignored files"
		return rep, nil
	}

	var entries []ArtifactEntry
	for _, rel := range files {
		src := filepath.Join(target, filepath.FromSlash(rel))
		if err := refuseUnsafeSource(target, src); err != nil {
			return nil, err
		}
		sum, size, err := hashFile(src)
		if err != nil {
			return nil, fmt.Errorf("artifact-archive: hash %s: %w", rel, err)
		}
		ent := ArtifactEntry{Path: rel, Digest: sum, Size: size, Kind: ClassifyArtifactPath(rel)}
		if req.Act {
			if err := writeObject(archive, src, sum); err != nil {
				return nil, fmt.Errorf("artifact-archive: store %s: %w", rel, err)
			}
			if afterArchiveHook != nil {
				afterArchiveHook(src)
			}
			again, _, err := hashFile(src)
			if err != nil || again != sum {
				return nil, fmt.Errorf("artifact-archive: drift on %s before removal (archived %s, now %s)", rel, sum, again)
			}
		}
		entries = append(entries, ent)
	}
	rep.Entries = entries
	rep.Archived = len(entries)

	if !req.Act {
		rep.Reason = "dry-run: archived nothing, removed nothing"
		return rep, nil
	}

	manPath, err := writeManifest(archive, relTarget, entries)
	if err != nil {
		return nil, err
	}
	rep.Manifest = filepath.ToSlash(relPathOrAbs(root, manPath))

	for _, ent := range entries {
		src := filepath.Join(target, filepath.FromSlash(ent.Path))
		now, _, err := hashFile(src)
		if err != nil {
			return nil, fmt.Errorf("artifact-archive: rehash %s: %w", ent.Path, err)
		}
		if now != ent.Digest {
			return nil, fmt.Errorf("artifact-archive: drift on %s at removal (archived %s, now %s)", ent.Path, ent.Digest, now)
		}
		obj := objectPath(archive, ent.Digest)
		back, _, err := hashFile(obj)
		if err != nil || back != ent.Digest {
			return nil, fmt.Errorf("artifact-archive: archive readback failed for %s; source kept", ent.Path)
		}
		if err := os.Remove(src); err != nil {
			return nil, fmt.Errorf("artifact-archive: remove %s: %w", ent.Path, err)
		}
		rep.Removed++
	}
	return rep, nil
}

func listIgnoredFiles(target string) ([]string, error) {
	cmd := exec.Command("git", "-C", target, "status", "--porcelain", "--untracked-files=all", "--ignored")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("artifact-archive: git status --ignored: %w", err)
	}
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "!! ") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "!! "))
		p = strings.Trim(p, "\"")
		if p == "" || strings.HasSuffix(p, "/") {
			continue
		}
		files = append(files, filepath.ToSlash(p))
	}
	return files, nil
}

func refuseActiveOwner(target string, now time.Time) error {
	raw, err := os.ReadFile(filepath.Join(target, "TASK-CONTEXT.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("artifact-archive: owner probe: %w", err)
	}
	var probe struct {
		TaskRef   string    `json:"task_ref"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return fmt.Errorf("artifact-archive: owner probe: %w", err)
	}
	if !probe.ExpiresAt.IsZero() && probe.ExpiresAt.After(now) {
		ref := strings.TrimSpace(probe.TaskRef)
		if ref == "" {
			ref = "unknown"
		}
		return fmt.Errorf("artifact-archive: active owner %s expires %s; refusing", ref, probe.ExpiresAt.UTC().Format(time.RFC3339))
	}
	return nil
}

func requireRegisteredWorktree(root, target string) error {
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return fmt.Errorf("artifact-archive: git worktree list: %w", err)
	}
	want, _ := filepath.EvalSymlinks(target)
	if want == "" {
		want = target
	}
	for _, line := range strings.Split(string(out), "\n") {
		if !strings.HasPrefix(line, "worktree ") {
			continue
		}
		p := strings.TrimPrefix(line, "worktree ")
		got, _ := filepath.EvalSymlinks(p)
		if got == "" {
			got = p
		}
		if sameArchivePath(got, want) {
			return nil
		}
	}
	return fmt.Errorf("artifact-archive: %s is not a registered git worktree of this repository", target)
}

func refuseUnsafeSource(root, src string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return fmt.Errorf("artifact-archive: stat %s: %w", src, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("artifact-archive: refusing symlink %s", src)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("artifact-archive: refusing non-regular %s", src)
	}
	abs, err := filepath.Abs(src)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("artifact-archive: path escapes target: %s", src)
	}
	return nil
}

func hashFile(path string) (string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func objectPath(archive, digest string) string {
	if len(digest) < 4 {
		return filepath.Join(archive, "objects", "sha256", digest)
	}
	return filepath.Join(archive, "objects", "sha256", digest[:2], digest)
}

func writeObject(archive, src, digest string) error {
	dst := objectPath(archive, digest)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(dst); err == nil {
		got, _, err := hashFile(dst)
		if err != nil {
			return err
		}
		if got != digest {
			return fmt.Errorf("object %s already exists with a different digest", digest)
		}
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		_ = os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	got, _, err := hashFile(dst)
	if err != nil {
		return err
	}
	if got != digest {
		_ = os.Remove(dst)
		return fmt.Errorf("object readback digest %s != %s", got, digest)
	}
	return nil
}

func writeManifest(archive, target string, entries []ArtifactEntry) (string, error) {
	manDir := filepath.Join(archive, "manifests")
	if err := os.MkdirAll(manDir, 0o700); err != nil {
		return "", err
	}
	body, err := json.MarshalIndent(ArtifactManifest{Version: artifactArchiveVersion, Target: target, Entries: entries}, "", "  ")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	name := hex.EncodeToString(sum[:]) + ".json"
	path := filepath.Join(manDir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

func absDir(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is required")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !fi.IsDir() {
		return "", fmt.Errorf("%s is not a directory", abs)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

func sameArchivePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil {
		ra = a
	}
	if errB != nil {
		rb = b
	}
	return filepath.Clean(ra) == filepath.Clean(rb)
}

func relPathOrAbs(root, p string) string {
	rel, err := filepath.Rel(root, p)
	if err != nil || strings.HasPrefix(rel, "..") {
		return p
	}
	return rel
}
