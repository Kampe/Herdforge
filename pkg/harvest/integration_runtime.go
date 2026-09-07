package harvest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

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

// HerdRuntimeInstaller installs an already-built, exact landed Herdforge
// executable. Building occurs in the owned source worktree, never the shared
// checkout. This does not implement a consumer application's deployment.
type HerdRuntimeInstaller struct {
	Root     string
	Source   string
	Revision string
}

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
	if st, err := os.Lstat(target); err == nil {
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("runtime bind: refusing to replace a non-regular target")
		}
		prior, err := provenance.ReadExecutable(target, r.Source)
		if err != nil || !prior.Comparable || !fullIntegrationSHA(prior.BinaryRevision) {
			return nil, fmt.Errorf("runtime bind: prior executable identity is unknown")
		}
		// A concurrent newer install, or unrelated binary, must never be downgraded.
		if err := gitroot.RequireAncestorContext(ctx, r.Root, prior.BinaryRevision, r.Revision); err != nil {
			return nil, fmt.Errorf("runtime bind: refusing runtime downgrade or unrelated history")
		}
		if err := r.preserve(target); err != nil {
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
	return r.Observe(ctx)
}

func (r HerdRuntimeInstaller) preserve(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	dir := filepath.Join(r.Root, ".herd", "runtime-previous")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	backup := filepath.Join(dir, hex.EncodeToString(h.Sum(nil)))
	if err := os.Link(path, backup); err != nil {
		if !os.IsExist(err) {
			return err
		}
		a, e1 := os.Stat(path)
		b, e2 := os.Stat(backup)
		if e1 != nil || e2 != nil || !os.SameFile(a, b) {
			return fmt.Errorf("runtime bind: existing backup does not preserve prior inode")
		}
	}
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
