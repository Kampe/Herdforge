package confinement

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrOSUnavailable is returned when production requires OS write isolation
// and no backend can prove denials on this host.
var ErrOSUnavailable = errors.New("confinement: OS write confinement backend unavailable")

// ErrOSProbeFailed is returned when a hermetic denial probe still created an
// outside inode — the sandbox is not effective.
var ErrOSProbeFailed = errors.New("confinement: OS write confinement probe failed")

// OSBackend isolates helper children that exercise write paths. It is the
// production proof that absolute shared-root and sibling writes are denied
// below the policy layer. It does not claim to wrap an interactive Herdr
// agent until the agent argv/PATH is routed through Wrap.
type OSBackend interface {
	Name() string
	Available() bool
	// ProveWriteDenials runs hermetic children that must fail to create
	// files outside worktree (shared-root incident path + sibling).
	ProveWriteDenials(worktree, sharedRoot string) error
	// Wrap rewrites cmd to run under the OS sandbox with worktree write scope.
	Wrap(cmd *exec.Cmd, worktree, sharedRoot string) error
}

// ActiveOS returns a live backend or nil when none is usable.
func ActiveOS() OSBackend {
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			return DarwinSeatbelt{}
		}
	}
	return nil
}

// RequireOS fails closed when OS write confinement cannot be proven.
func RequireOS() (OSBackend, error) {
	b := ActiveOS()
	if b == nil || !b.Available() {
		return nil, ErrOSUnavailable
	}
	return b, nil
}

// FakeOS is a test-only backend that records wrap calls and always "proves"
// denials without touching sandbox-exec. Production never uses it.
type FakeOS struct {
	Proved bool
	Wrapped int
}

func (f *FakeOS) Name() string     { return "fake-os" }
func (f *FakeOS) Available() bool  { return true }
func (f *FakeOS) ProveWriteDenials(worktree, sharedRoot string) error {
	if strings.TrimSpace(worktree) == "" {
		return ErrOSProbeFailed
	}
	f.Proved = true
	return nil
}
func (f *FakeOS) Wrap(cmd *exec.Cmd, worktree, sharedRoot string) error {
	if cmd == nil {
		return fmt.Errorf("confinement: nil cmd")
	}
	f.Wrapped++
	return nil
}

// DarwinSeatbelt uses macOS sandbox-exec with a deny-default file-write profile
// that only re-allows the authenticated worktree.
type DarwinSeatbelt struct{}

func (DarwinSeatbelt) Name() string { return "sandbox-exec" }
func (DarwinSeatbelt) Available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

func (d DarwinSeatbelt) ProveWriteDenials(worktree, sharedRoot string) error {
	if !d.Available() {
		return ErrOSUnavailable
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return err
	}
	absShared := ""
	if strings.TrimSpace(sharedRoot) != "" {
		absShared, err = realPath(sharedRoot)
		if err != nil {
			return err
		}
	}
	profile, err := writeSeatbeltProfile(absWT, absShared)
	if err != nil {
		return err
	}

	// Outside targets mirror the FAC-188 incident topology: absolute write under
	// the shared root (parent of the task worktree) and a sibling worktree path.
	// Both must remain non-existent after the sandboxed attempt.
	outside := filepath.Join(absShared, ".herd", "FAC-188-R2-RESIDUAL.md")
	if absShared == "" {
		// Fall back to a disposable parent sibling when shared root is unknown.
		outside = filepath.Join(filepath.Dir(absWT), "FAC-188-R2-RESIDUAL.md")
	}
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		return err
	}
	_ = os.Remove(outside)
	siblingRoot := filepath.Join(filepath.Dir(absWT), "sibling-worktree-fac190")
	if err := os.MkdirAll(siblingRoot, 0o755); err != nil {
		return err
	}
	defer os.RemoveAll(siblingRoot)
	sibling := filepath.Join(siblingRoot, "escape.go")
	_ = os.Remove(sibling)

	// Denied: absolute write outside worktree.
	if err := d.runUnder(profile, absWT, []string{"/bin/sh", "-c", "echo pwned > " + shellQuote(outside)}); err == nil {
		if _, statErr := os.Stat(outside); statErr == nil {
			return fmt.Errorf("%w: outside absolute write succeeded", ErrOSProbeFailed)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		return fmt.Errorf("%w: outside inode created", ErrOSProbeFailed)
	}

	// Denied: sibling path write.
	if err := d.runUnder(profile, absWT, []string{"/bin/sh", "-c", "echo pwned > " + shellQuote(sibling)}); err == nil {
		if _, statErr := os.Stat(sibling); statErr == nil {
			return fmt.Errorf("%w: sibling write succeeded", ErrOSProbeFailed)
		}
	}
	if _, err := os.Stat(sibling); err == nil {
		return fmt.Errorf("%w: sibling inode created", ErrOSProbeFailed)
	}

	// Allowed: write inside the authenticated worktree.
	inside := filepath.Join(absWT, ".herd", "confine-probe-ok")
	_ = os.Remove(inside)
	if err := d.runUnder(profile, absWT, []string{"/bin/sh", "-c", "echo ok > " + shellQuote(inside)}); err != nil {
		return fmt.Errorf("%w: in-worktree write denied: %v", ErrOSProbeFailed, err)
	}
	if data, err := os.ReadFile(inside); err != nil || !strings.Contains(string(data), "ok") {
		return fmt.Errorf("%w: in-worktree write missing", ErrOSProbeFailed)
	}
	_ = os.Remove(inside)
	return nil
}

func (d DarwinSeatbelt) Wrap(cmd *exec.Cmd, worktree, sharedRoot string) error {
	if cmd == nil {
		return fmt.Errorf("confinement: nil cmd")
	}
	if !d.Available() {
		return ErrOSUnavailable
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return err
	}
	absShared := ""
	if strings.TrimSpace(sharedRoot) != "" {
		absShared, _ = realPath(sharedRoot)
	}
	profile, err := writeSeatbeltProfile(absWT, absShared)
	if err != nil {
		return err
	}
	origPath := cmd.Path
	if origPath == "" && len(cmd.Args) > 0 {
		origPath = cmd.Args[0]
	}
	args := []string{"-f", profile}
	args = append(args, origPath)
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec"}, args...)
	return nil
}

func (d DarwinSeatbelt) runUnder(profile, worktree string, argv []string) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty argv")
	}
	cmd := exec.Command("/usr/bin/sandbox-exec", append([]string{"-f", profile}, argv...)...)
	cmd.Dir = worktree
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func writeSeatbeltProfile(worktree, sharedRoot string) (string, error) {
	// Deny-default; re-allow process/read broadly so /bin/sh probes run, but
	// file-write only under the authenticated worktree. Do NOT re-allow the
	// host temp tree — probe outside targets live there and must stay denied.
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	b.WriteString("(allow process*)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach*)\n")
	b.WriteString("(allow signal)\n")
	b.WriteString("(allow file-read*)\n")
	b.WriteString("(allow file-ioctl)\n")
	b.WriteString("(allow file-write-data (literal \"/dev/null\"))\n")
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", worktree)
	// Last-match wins for overlapping subpaths on some macOS versions: put
	// explicit shared-root deny after the worktree allow when shared is a
	// parent of the worktree (the FAC-188 incident topology).
	if sharedRoot != "" && sharedRoot != worktree {
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", sharedRoot)
		// Re-allow the worktree after the parent deny so nested task trees work.
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", worktree)
	}
	// Profile file itself must live under worktree so sandbox-exec can read it
	// without a global temp write grant for the probe children.
	dir := filepath.Join(worktree, ".herd", "confine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "profile.sb")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func shellQuote(path string) string {
	return "'" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
}

// realPath resolves symlinks so seatbelt subpath rules match the kernel view
// (/var/folders vs /private/var/folders on macOS).
func realPath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return abs, nil
	}
	return resolved, nil
}
