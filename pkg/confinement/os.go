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

// OSBackend isolates helper children that exercise write paths and installs
// the durable agent wrapper that production launches must place first on PATH.
type OSBackend interface {
	Name() string
	Available() bool
	// Prepare writes the durable seatbelt profile under the worktree and
	// returns its absolute path. The profile is first-match safe: only the
	// worktree is granted file-write; shared-root parents are never allowed.
	Prepare(worktree, sharedRoot string) (profilePath string, err error)
	// ProveWriteDenials runs hermetic children under profilePath. It never
	// creates directories under sharedRoot; outside targets live in a disposable
	// probe tree that is removed after the proof.
	ProveWriteDenials(worktree, sharedRoot, profilePath string) error
	// Wrap rewrites cmd to run under sandbox-exec with the prepared profile.
	Wrap(cmd *exec.Cmd, profilePath string) error
	// InstallAgentWrapper installs a PATH-first wrapper named `kind` that
	// re-execs the real agent under the same profile used by ProveWriteDenials.
	InstallAgentWrapper(worktree, profilePath, kind, realAgentPath string) (binDir string, err error)
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

// FakeOS is a test-only backend. Production never uses it.
type FakeOS struct {
	Proved  bool
	Wrapped int
	BinDir  string
}

func (f *FakeOS) Name() string    { return "fake-os" }
func (f *FakeOS) Available() bool { return true }

func (f *FakeOS) Prepare(worktree, sharedRoot string) (string, error) {
	if strings.TrimSpace(worktree) == "" {
		return "", ErrOSProbeFailed
	}
	dir := filepath.Join(worktree, ".herd", "confine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "profile.sb")
	if err := os.WriteFile(path, []byte("(version 1)\n(deny default)\n"), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (f *FakeOS) ProveWriteDenials(worktree, sharedRoot, profilePath string) error {
	if strings.TrimSpace(worktree) == "" || strings.TrimSpace(profilePath) == "" {
		return ErrOSProbeFailed
	}
	f.Proved = true
	return nil
}

func (f *FakeOS) Wrap(cmd *exec.Cmd, profilePath string) error {
	if cmd == nil || profilePath == "" {
		return fmt.Errorf("confinement: nil cmd or empty profile")
	}
	f.Wrapped++
	return nil
}

func (f *FakeOS) InstallAgentWrapper(worktree, profilePath, kind, realAgentPath string) (string, error) {
	if kind == "" || profilePath == "" {
		return "", fmt.Errorf("confinement: kind and profile required for agent wrapper")
	}
	binDir := filepath.Join(worktree, ".herd", "confine", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	wrapper := filepath.Join(binDir, kind)
	// Test wrapper records invocations; does not need sandbox-exec.
	body := "#!/bin/sh\nexec " + shellSingleArg(realAgentPath) + " \"$@\"\n"
	if realAgentPath == "" {
		body = "#!/bin/sh\nexit 0\n"
	}
	if err := os.WriteFile(wrapper, []byte(body), 0o755); err != nil {
		return "", err
	}
	f.BinDir = binDir
	return binDir, nil
}

// DarwinSeatbelt uses macOS sandbox-exec with a deny-default file-write profile
// that only re-allows the authenticated worktree (first-match safe).
type DarwinSeatbelt struct{}

func (DarwinSeatbelt) Name() string { return "sandbox-exec" }
func (DarwinSeatbelt) Available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

func (d DarwinSeatbelt) Prepare(worktree, sharedRoot string) (string, error) {
	if !d.Available() {
		return "", ErrOSUnavailable
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return "", err
	}
	// sharedRoot is accepted for validation only; the profile never grants it.
	if strings.TrimSpace(sharedRoot) != "" {
		if _, err := realPath(sharedRoot); err != nil {
			return "", err
		}
	}
	return writeSeatbeltProfile(absWT)
}

func (d DarwinSeatbelt) ProveWriteDenials(worktree, sharedRoot, profilePath string) error {
	if !d.Available() {
		return ErrOSUnavailable
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return err
	}
	if strings.TrimSpace(profilePath) == "" {
		return fmt.Errorf("%w: empty profile", ErrOSProbeFailed)
	}
	// Hermetic outside targets: disposable tree OUTSIDE the worktree and NOT
	// under the live shared root. Creating dirs under sharedRoot is forbidden
	// for a proof function (FAC-190 review finding #2).
	probeRoot, err := os.MkdirTemp(filepath.Dir(absWT), "fac190-probe-*")
	if err != nil {
		// Fall back to system temp when parent of worktree is not writable
		// (should not happen for task worktrees under .herd/worktrees).
		probeRoot, err = os.MkdirTemp("", "fac190-probe-*")
		if err != nil {
			return err
		}
	}
	probeRoot, err = realPath(probeRoot)
	if err != nil {
		_ = os.RemoveAll(probeRoot)
		return err
	}
	// Refuse to place probes under the shared root or worktree.
	if sharedRoot != "" {
		absShared, err := realPath(sharedRoot)
		if err != nil {
			_ = os.RemoveAll(probeRoot)
			return err
		}
		if probeRoot == absShared || isPathPrefix(probeRoot, absShared) || isPathPrefix(absShared, probeRoot) {
			// Recreate on system temp if we accidentally nested under shared.
			_ = os.RemoveAll(probeRoot)
			probeRoot, err = os.MkdirTemp("", "fac190-probe-*")
			if err != nil {
				return err
			}
			probeRoot, err = realPath(probeRoot)
			if err != nil {
				_ = os.RemoveAll(probeRoot)
				return err
			}
		}
	}
	if isPathPrefix(probeRoot, absWT) || isPathPrefix(absWT, probeRoot) {
		_ = os.RemoveAll(probeRoot)
		return fmt.Errorf("%w: probe root collides with worktree", ErrOSProbeFailed)
	}
	defer os.RemoveAll(probeRoot)

	outside := filepath.Join(probeRoot, "shared-root", ".herd", "FAC-188-R2-RESIDUAL.md")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		return err
	}
	sibling := filepath.Join(probeRoot, "sibling-worktree", "escape.go")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		return err
	}

	// Denied: absolute write outside worktree (no shell; tee argv only).
	if err := d.writeUnder(profilePath, absWT, outside, "pwned\n"); err == nil {
		if _, statErr := os.Stat(outside); statErr == nil {
			return fmt.Errorf("%w: outside absolute write succeeded", ErrOSProbeFailed)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		_ = os.Remove(outside)
		return fmt.Errorf("%w: outside inode created", ErrOSProbeFailed)
	}

	// Denied: sibling path write.
	if err := d.writeUnder(profilePath, absWT, sibling, "pwned\n"); err == nil {
		if _, statErr := os.Stat(sibling); statErr == nil {
			return fmt.Errorf("%w: sibling write succeeded", ErrOSProbeFailed)
		}
	}
	if _, err := os.Stat(sibling); err == nil {
		_ = os.Remove(sibling)
		return fmt.Errorf("%w: sibling inode created", ErrOSProbeFailed)
	}

	// Allowed: write inside the authenticated worktree.
	inside := filepath.Join(absWT, ".herd", "confine-probe-ok")
	_ = os.Remove(inside)
	if err := d.writeUnder(profilePath, absWT, inside, "ok\n"); err != nil {
		return fmt.Errorf("%w: in-worktree write denied: %v", ErrOSProbeFailed, err)
	}
	data, err := os.ReadFile(inside)
	if err != nil || !strings.Contains(string(data), "ok") {
		return fmt.Errorf("%w: in-worktree write missing", ErrOSProbeFailed)
	}
	_ = os.Remove(inside)
	return nil
}

func (d DarwinSeatbelt) Wrap(cmd *exec.Cmd, profilePath string) error {
	if cmd == nil {
		return fmt.Errorf("confinement: nil cmd")
	}
	if !d.Available() {
		return ErrOSUnavailable
	}
	if strings.TrimSpace(profilePath) == "" {
		return fmt.Errorf("confinement: empty profile")
	}
	origPath := cmd.Path
	if origPath == "" && len(cmd.Args) > 0 {
		origPath = cmd.Args[0]
	}
	args := []string{"-f", profilePath, origPath}
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec"}, args...)
	return nil
}

func (d DarwinSeatbelt) InstallAgentWrapper(worktree, profilePath, kind, realAgentPath string) (string, error) {
	if !d.Available() {
		return "", ErrOSUnavailable
	}
	if strings.TrimSpace(kind) == "" || strings.TrimSpace(profilePath) == "" {
		return "", fmt.Errorf("confinement: kind and profile required")
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return "", err
	}
	absProfile, err := realPath(profilePath)
	if err != nil {
		return "", err
	}
	real := strings.TrimSpace(realAgentPath)
	if real == "" || !filepath.IsAbs(real) {
		// Resolve bare kind/argv[0] names through PATH before realPath.
		lookup := real
		if lookup == "" {
			lookup = kind
		}
		found, lerr := exec.LookPath(lookup)
		if lerr != nil {
			return "", fmt.Errorf("confinement: resolve agent %q: %w", lookup, lerr)
		}
		real = found
	}
	resolved, err := realPath(real)
	if err != nil {
		return "", err
	}
	real = resolved
	binDir := filepath.Join(absWT, ".herd", "confine", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	// Pure argv re-exec: no shell interpolation of paths.
	// sandbox-exec -f profile real-agent "$@"
	script := fmt.Sprintf("#!/bin/sh\n# FAC-190 agent wrap — same profile as ProveWriteDenials.\nexec /usr/bin/sandbox-exec -f %s %s \"$@\"\n",
		shellSingleArg(absProfile), shellSingleArg(real))
	wrapper := filepath.Join(binDir, kind)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		return "", err
	}
	return binDir, nil
}

// writeUnder runs /usr/bin/tee under sandbox-exec (no shell, path is argv).
func (d DarwinSeatbelt) writeUnder(profile, worktree, target, content string) error {
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", profile, "/usr/bin/tee", target)
	cmd.Dir = worktree
	cmd.Stdin = strings.NewReader(content)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// writeSeatbeltProfile emits a first-match-safe profile:
// deny default, then allow file-write only under the authenticated worktree.
// The shared parent is NEVER granted, so parent/child topology cannot invert
// deny/allow order on TrustedBSD first-match evaluation.
func writeSeatbeltProfile(worktree string) (string, error) {
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
	// Sole write grant: authenticated worktree. First-match: any path outside
	// this subpath falls through to (deny default).
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", worktree)

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

// shellSingleArg quotes a path for a single shell word inside a wrapper script
// that we author (not for /bin/sh -c with user input). Paths are absolute
// realPath results; newlines are rejected fail-closed.
func shellSingleArg(path string) string {
	if strings.ContainsAny(path, "\n\r\x00") {
		// Caller must have rejected via realPath; defensive placeholder.
		return "''"
	}
	return "'" + strings.ReplaceAll(path, "'", `'"'"'`) + "'"
}

// realPath resolves symlinks so seatbelt subpath rules match the kernel view.
// EvalSymlinks failures fail closed — never return an unresolved path.
func realPath(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("confinement: empty path")
	}
	if strings.ContainsAny(path, "\n\r\x00") {
		return "", fmt.Errorf("confinement: path contains control characters")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("confinement: resolve path %q: %w", abs, err)
	}
	return resolved, nil
}

func isPathPrefix(child, parent string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
