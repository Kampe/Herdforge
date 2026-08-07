package confinement

import (
	"crypto/sha256"
	"encoding/hex"
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
	// returns its absolute path. First-match safe: worktree + this worktree's
	// gitdir + temp + network are granted; shared-root residual paths are not.
	Prepare(worktree, sharedRoot string) (profilePath string, err error)
	// ProveWriteDenials runs children under profilePath: denies shared-root
	// residual writes, allows in-worktree and linked-gitdir writes. Never
	// creates directories under sharedRoot.
	ProveWriteDenials(worktree, sharedRoot, profilePath string) error
	// Wrap rewrites cmd to run under sandbox-exec with the prepared profile.
	Wrap(cmd *exec.Cmd, profilePath string) error
	// InstallAgentWrappers installs PATH-first wrappers for every name in
	// `names` (provider and/or argv[0] basenames) that re-exec the real agent
	// under the same profile used by ProveWriteDenials.
	InstallAgentWrappers(worktree, profilePath string, names []string, realAgentPath string) (binDir string, err error)
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
	// Fake profile still encodes grants so digest/bind tests have real content.
	body := "(version 1)\n(deny default)\n(allow file-write* (subpath \"" + worktree + "\"))\n"
	if sharedRoot != "" {
		body += "; shared=" + sharedRoot + "\n"
	}
	dir := filepath.Join(worktree, ".herd", "confine")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "profile.sb")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}

func (f *FakeOS) ProveWriteDenials(worktree, sharedRoot, profilePath string) error {
	if strings.TrimSpace(worktree) == "" || strings.TrimSpace(profilePath) == "" {
		return ErrOSProbeFailed
	}
	// Non-vacuous without sandbox-exec: profile must exist and look like a
	// write-confine profile, and must not grant the entire shared root.
	data, err := os.ReadFile(profilePath)
	if err != nil || len(data) == 0 {
		return fmt.Errorf("%w: empty or missing profile", ErrOSProbeFailed)
	}
	body := string(data)
	if !strings.Contains(body, "file-write") || !strings.Contains(body, "deny default") {
		return fmt.Errorf("%w: profile missing deny-default write policy", ErrOSProbeFailed)
	}
	if sharedRoot != "" {
		// Match either raw or cleaned absolute forms of the shared root grant.
		abs, _ := filepath.Abs(sharedRoot)
		for _, root := range []string{sharedRoot, abs, filepath.Clean(sharedRoot)} {
			if root == "" {
				continue
			}
			grant := "(allow file-write* (subpath \"" + root + "\"))"
			if strings.Contains(body, grant) {
				return fmt.Errorf("%w: profile grants whole shared root", ErrOSProbeFailed)
			}
		}
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

func (f *FakeOS) InstallAgentWrappers(worktree, profilePath string, names []string, realAgentPath string) (string, error) {
	if len(names) == 0 || profilePath == "" {
		return "", fmt.Errorf("confinement: wrapper names and profile required")
	}
	binDir := filepath.Join(worktree, ".herd", "confine", "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	digest, _ := ProfileDigest(profilePath)
	// Embed path + content digest so VerifyAgentWrappers binds install to prove.
	body := "#!/bin/sh\n# profile=" + profilePath + "\n# profile_digest=" + digest + "\nexec " + shellSingleArg(realAgentPath) + " \"$@\"\n"
	if realAgentPath == "" {
		body = "#!/bin/sh\n# profile=" + profilePath + "\n# profile_digest=" + digest + "\nexit 0\n"
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, string(filepath.Separator)) {
			return "", fmt.Errorf("confinement: invalid wrapper name %q", name)
		}
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(body), 0o755); err != nil {
			return "", err
		}
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
	absShared := ""
	if strings.TrimSpace(sharedRoot) != "" {
		absShared, err = realPath(sharedRoot)
		if err != nil {
			return "", err
		}
	}
	gitDir, err := absoluteGitDir(absWT)
	if err != nil {
		return "", fmt.Errorf("confinement: resolve worktree gitdir: %w", err)
	}
	commonDir, err := absoluteGitCommonDir(absWT)
	if err != nil {
		return "", fmt.Errorf("confinement: resolve git common-dir: %w", err)
	}
	// Never grant the entire shared checkout as a write root.
	if absShared != "" && (gitDir == absShared || commonDir == absShared) {
		return "", fmt.Errorf("confinement: refusing profile that would grant whole shared root")
	}
	return writeSeatbeltProfile(absWT, gitDir, commonDir)
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
	// Hermetic outside targets: ALWAYS system temp (never under shared).
	probeRoot, err := os.MkdirTemp("", "fac190-probe-*")
	if err != nil {
		return err
	}
	probeRoot, err = realPath(probeRoot)
	if err != nil {
		_ = os.RemoveAll(probeRoot)
		return err
	}
	defer os.RemoveAll(probeRoot)
	if isPathPrefix(probeRoot, absWT) || isPathPrefix(absWT, probeRoot) {
		return fmt.Errorf("%w: probe root collides with worktree", ErrOSProbeFailed)
	}

	outside := filepath.Join(probeRoot, "shared-root", ".herd", "FAC-188-R2-RESIDUAL.md")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		return err
	}
	sibling := filepath.Join(probeRoot, "sibling-worktree", "escape.go")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		return err
	}

	// Denied: absolute write outside worktree (hermetic).
	if err := d.writeUnder(profilePath, absWT, outside, "pwned\n"); err == nil {
		if _, statErr := os.Stat(outside); statErr == nil {
			return fmt.Errorf("%w: outside absolute write succeeded", ErrOSProbeFailed)
		}
	}
	if _, err := os.Stat(outside); err == nil {
		_ = os.Remove(outside)
		return fmt.Errorf("%w: outside inode created", ErrOSProbeFailed)
	}
	if err := d.writeUnder(profilePath, absWT, sibling, "pwned\n"); err == nil {
		if _, statErr := os.Stat(sibling); statErr == nil {
			return fmt.Errorf("%w: sibling write succeeded", ErrOSProbeFailed)
		}
	}

	// Denied: FAC-188 incident path under the real shared root (no MkdirAll).
	// Parent shared root already exists; tee cannot create intermediate dirs.
	if strings.TrimSpace(sharedRoot) != "" {
		absShared, err := realPath(sharedRoot)
		if err != nil {
			return err
		}
		if probeRoot == absShared || isPathPrefix(probeRoot, absShared) || isPathPrefix(absShared, probeRoot) {
			return fmt.Errorf("%w: probe root collides with shared root (check TMPDIR)", ErrOSProbeFailed)
		}
		// Direct child of shared root — no new directories created if denied.
		incidentProbe := filepath.Join(absShared, "FAC190_DENY_PROBE")
		_ = os.Remove(incidentProbe)
		if err := d.writeUnder(profilePath, absWT, incidentProbe, "pwned\n"); err == nil {
			if _, statErr := os.Stat(incidentProbe); statErr == nil {
				_ = os.Remove(incidentProbe)
				return fmt.Errorf("%w: shared-root residual write succeeded", ErrOSProbeFailed)
			}
		}
		if _, err := os.Stat(incidentProbe); err == nil {
			_ = os.Remove(incidentProbe)
			return fmt.Errorf("%w: shared-root residual inode created", ErrOSProbeFailed)
		}
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

	// Allowed: linked gitdir metadata.
	gitDir, err := absoluteGitDir(absWT)
	if err != nil {
		return fmt.Errorf("%w: gitdir: %v", ErrOSProbeFailed, err)
	}
	gitProbe := filepath.Join(gitDir, "fac190-confine-probe.lock")
	_ = os.Remove(gitProbe)
	if err := d.writeUnder(profilePath, absWT, gitProbe, "ok\n"); err != nil {
		return fmt.Errorf("%w: linked gitdir write denied: %v", ErrOSProbeFailed, err)
	}
	_ = os.Remove(gitProbe)

	// Allowed: agent-shaped git object write into common-dir (no empty commit).
	// This is the real "agents can land work" gate; tee into gitdir alone is
	// insufficient (objects live under common .git/objects).
	if err := d.proveGitObjectWrite(profilePath, absWT); err != nil {
		return err
	}

	// Denied: git hooks under common-dir when common-dir is outside the
	// worktree (linked worktree topology). If common-dir is nested inside the
	// worktree (standalone git init fixture), hooks fall under the worktree
	// grant — production Herdforge task worktrees are linked, not nested.
	commonDir, err := absoluteGitCommonDir(absWT)
	if err != nil {
		return fmt.Errorf("%w: common-dir: %v", ErrOSProbeFailed, err)
	}
	if !isPathPrefix(commonDir, absWT) && filepath.Clean(commonDir) != absWT {
		hook := filepath.Join(commonDir, "hooks", "fac190-deny-hook")
		_ = os.Remove(hook)
		if err := d.writeUnder(profilePath, absWT, hook, "evil\n"); err == nil {
			if _, statErr := os.Stat(hook); statErr == nil {
				_ = os.Remove(hook)
				return fmt.Errorf("%w: git hook write under common-dir succeeded", ErrOSProbeFailed)
			}
		}
		if _, err := os.Stat(hook); err == nil {
			_ = os.Remove(hook)
			return fmt.Errorf("%w: git hook inode created", ErrOSProbeFailed)
		}
	}
	return nil
}

// proveGitObjectWrite runs `git hash-object -w` under the profile with TMPDIR
// inside the worktree — proves common-dir object store writes without leaving
// a branch commit.
func (d DarwinSeatbelt) proveGitObjectWrite(profile, worktree string) error {
	tmpDir := filepath.Join(worktree, ".herd", "confine", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return err
	}
	cmd := exec.Command("/usr/bin/sandbox-exec", "-f", profile, "git", "-C", worktree, "hash-object", "-w", "--stdin")
	cmd.Dir = worktree
	cmd.Env = []string{
		"PATH=/usr/bin:/bin:/usr/local/bin",
		"TMPDIR=" + tmpDir,
		"TMP=" + tmpDir,
		"TEMP=" + tmpDir,
		"GIT_CONFIG_NOSYSTEM=1",
		"HOME=" + tmpDir,
	}
	cmd.Stdin = strings.NewReader("fac190-confine-object-probe\n")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: git hash-object -w denied (agents cannot store objects): %v (%s)",
			ErrOSProbeFailed, err, strings.TrimSpace(string(out)))
	}
	oid := strings.TrimSpace(string(out))
	if len(oid) < 40 {
		return fmt.Errorf("%w: git hash-object produced no oid", ErrOSProbeFailed)
	}
	// Best-effort cleanup of the probe object (not required for proof validity).
	obj := filepath.Join(worktree, ".git", "objects", oid[:2], oid[2:])
	if common, err := absoluteGitCommonDir(worktree); err == nil {
		obj = filepath.Join(common, "objects", oid[:2], oid[2:])
	}
	_ = os.Remove(obj)
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

func (d DarwinSeatbelt) InstallAgentWrappers(worktree, profilePath string, names []string, realAgentPath string) (string, error) {
	if !d.Available() {
		return "", ErrOSUnavailable
	}
	if len(names) == 0 || strings.TrimSpace(profilePath) == "" {
		return "", fmt.Errorf("confinement: wrapper names and profile required")
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
		lookup := real
		if lookup == "" {
			lookup = names[0]
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
	digest, err := ProfileDigest(absProfile)
	if err != nil {
		return "", err
	}
	// Pure argv re-exec. Path + content digest bind wrapper to proved profile.
	script := fmt.Sprintf("#!/bin/sh\n# FAC-190 agent wrap\n# profile=%s\n# profile_digest=%s\nexec /usr/bin/sandbox-exec -f %s %s \"$@\"\n",
		absProfile, digest, shellSingleArg(absProfile), shellSingleArg(real))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, string(filepath.Separator)) {
			return "", fmt.Errorf("confinement: invalid wrapper name %q", name)
		}
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			return "", err
		}
	}
	return binDir, nil
}

// VerifyAgentWrappers fails closed when any expected wrapper is missing, not
// executable, or no longer embeds both the profile path and content digest.
func VerifyAgentWrappers(binDir, profilePath, profileDigest string, names []string) error {
	if strings.TrimSpace(binDir) == "" || strings.TrimSpace(profilePath) == "" || strings.TrimSpace(profileDigest) == "" || len(names) == 0 {
		return fmt.Errorf("confinement: wrapper verification inputs incomplete")
	}
	// Live profile bytes must still match the bound digest.
	got, err := ProfileDigest(profilePath)
	if err != nil {
		return err
	}
	if got != profileDigest {
		return fmt.Errorf("confinement: profile content digest mismatch")
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("confinement: empty wrapper name")
		}
		path := filepath.Join(binDir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("confinement: wrapper %q missing: %w", name, err)
		}
		body := string(data)
		if !strings.Contains(body, profilePath) {
			return fmt.Errorf("confinement: wrapper %q does not embed profile %q", name, profilePath)
		}
		if !strings.Contains(body, profileDigest) {
			return fmt.Errorf("confinement: wrapper %q does not embed profile digest", name)
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode()&0o111 == 0 {
			return fmt.Errorf("confinement: wrapper %q is not executable", name)
		}
	}
	return nil
}

// WrapperNames returns the distinct PATH entry names that must intercept the
// live agent: argv[0] basename (shell executable) and provider (herdr kind),
// when they differ (e.g. provider=ollama/lazer → argv0=opencode).
func WrapperNames(provider, argv0 string) []string {
	seen := map[string]struct{}{}
	var out []string
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		base := filepath.Base(s)
		if base == "" || base == "." || base == string(filepath.Separator) {
			return
		}
		if _, ok := seen[base]; ok {
			return
		}
		seen[base] = struct{}{}
		out = append(out, base)
	}
	add(argv0)
	add(provider)
	return out
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

// writeSeatbeltProfile emits an agent-viable profile that can still land git
// objects without opening shared-repo hook/config RCE.
//
// Linked worktrees need worktree + gitdir + common objects/refs/logs (not the
// whole common-dir: a blanket common-dir allow lets agents rewrite hooks even
// when a prior deny line is present — observed on Darwin TrustedBSD).
// Shared checkout root is never a write subpath grant.
func writeSeatbeltProfile(worktree, gitDir, commonDir string) (string, error) {
	if worktree == "" || gitDir == "" || commonDir == "" {
		return "", fmt.Errorf("confinement: worktree, gitdir, and common-dir required for profile")
	}
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	b.WriteString("(allow process*)\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach*)\n")
	b.WriteString("(allow signal)\n")
	b.WriteString("(allow network*)\n")
	b.WriteString("(allow file-read*)\n")
	b.WriteString("(allow file-ioctl)\n")
	b.WriteString("(allow file-write-data (literal \"/dev/null\"))\n")
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", worktree)
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", gitDir)
	// Narrow common-dir grants — objects/refs/logs only (no hooks/, no config).
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", filepath.Join(commonDir, "objects"))
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", filepath.Join(commonDir, "refs"))
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", filepath.Join(commonDir, "logs"))
	fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", filepath.Join(commonDir, "info"))
	// Top-level git lock/state files (not under hooks/).
	for _, name := range []string{"packed-refs", "COMMIT_EDITMSG", "FETCH_HEAD", "ORIG_HEAD", "HEAD"} {
		fmt.Fprintf(&b, "(allow file-write* (literal %q))\n", filepath.Join(commonDir, name))
		fmt.Fprintf(&b, "(allow file-write* (literal %q))\n", filepath.Join(commonDir, name+".lock"))
	}
	b.WriteString("(allow file-write* (subpath \"/private/tmp\"))\n")
	b.WriteString("(allow file-write* (subpath \"/tmp\"))\n")

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

// absoluteGitDir returns the absolute git directory for a worktree (the
// linked-worktree metadata dir under .git/worktrees/<name> when applicable).
func absoluteGitDir(worktree string) (string, error) {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --absolute-git-dir: %w", err)
	}
	return realPath(strings.TrimSpace(string(out)))
}

// absoluteGitCommonDir returns the absolute common git directory (object store).
func absoluteGitCommonDir(worktree string) (string, error) {
	out, err := exec.Command("git", "-C", worktree, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --git-common-dir: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("empty git-common-dir")
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(worktree, path)
	}
	return realPath(path)
}

// ProfileDigest returns the SHA-256 of the profile file bytes.
func ProfileDigest(profilePath string) (string, error) {
	data, err := os.ReadFile(profilePath)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
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
