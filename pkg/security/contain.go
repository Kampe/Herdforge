package security

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Containment errors — fail closed when OS isolation is unavailable or weak.
var (
	ErrContainmentUnavailable = fmt.Errorf("security: OS process containment unavailable (fail-closed)")
	ErrContainmentProbeFailed = fmt.Errorf("security: containment probe failed — denials not effective")
)

// ContainmentBackend isolates a child process at the OS level.
type ContainmentBackend interface {
	Name() string
	Available() bool
	// Install prepares profiles/wrappers under worktree.
	// Returns PATH prefix (bin dir), seatbelt profile path, and env file path
	// (caller writes KEY=VALUE lines via WriteEnvFile before ProveDenials/launch).
	Install(worktree string, policy *LaunchPolicy, grant *LaunchGrant, kind, realAgentPath string) (pathPrefix, profilePath, envFilePath string, err error)
	// ProveDenials runs hermetic children in disposable dirs (never mutates
	// the real shared checkout or production package trees).
	ProveDenials(worktree string, policy *LaunchPolicy, grant *LaunchGrant, profilePath string, scrubbedEnv []string) error
	// WrapCmd mutates cmd to run under sandbox-exec with scrubbed env only.
	WrapCmd(cmd *exec.Cmd, profilePath string, scrubbedEnv []string) error
}

// ActiveContainment returns a backend that can actually Install+Prove, or nil.
// Linux bwrap is NOT advertised until a full Install path exists (FAC-133 audit).
func ActiveContainment() ContainmentBackend {
	if runtime.GOOS == "darwin" {
		if _, err := exec.LookPath("sandbox-exec"); err == nil {
			return DarwinSeatbelt{}
		}
	}
	return nil
}

// RequireContainment fails closed when no OS backend is available.
func RequireContainment() (ContainmentBackend, error) {
	b := ActiveContainment()
	if b == nil || !b.Available() {
		return nil, ErrContainmentUnavailable
	}
	return b, nil
}

// DarwinSeatbelt uses macOS sandbox-exec with deny-by-default read/exec policy.
type DarwinSeatbelt struct{}

func (DarwinSeatbelt) Name() string { return "sandbox-exec" }
func (DarwinSeatbelt) Available() bool {
	_, err := exec.LookPath("sandbox-exec")
	return err == nil
}

func (d DarwinSeatbelt) Install(worktree string, policy *LaunchPolicy, grant *LaunchGrant, kind, realAgentPath string) (string, string, string, error) {
	if !d.Available() {
		return "", "", "", ErrContainmentUnavailable
	}
	if policy == nil {
		return "", "", "", fmt.Errorf("%w: nil policy", ErrUnknownPolicy)
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return "", "", "", err
	}
	absShared, err := realPath(policy.SharedCheckout)
	if err != nil {
		return "", "", "", err
	}
	dir := filepath.Join(absWT, ".herd", "contain")
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", "", "", err
	}
	if realAgentPath == "" {
		realAgentPath, err = ResolveAgentBinary(kind)
		if err != nil {
			return "", "", "", fmt.Errorf("resolve agent binary: %w", err)
		}
	}
	if resolved, rerr := realPath(realAgentPath); rerr == nil {
		realAgentPath = resolved
	}

	profilePath := filepath.Join(dir, "profile.sb")
	profile := seatbeltProfileDenyDefault(absWT, absShared, realAgentPath, grant, policy)
	if err := os.WriteFile(profilePath, []byte(profile), 0o644); err != nil {
		return "", "", "", err
	}

	envFile := filepath.Join(dir, "env.list")
	// Placeholder until LaunchAgent writes the scrubbed env.
	if err := os.WriteFile(envFile, nil, 0o600); err != nil {
		return "", "", "", err
	}

	// Pure /bin/sh wrapper: preserve agent argv, load KEY=VALUE lines without
	// word-splitting (spaces and metacharacters in values stay intact as argv
	// elements to env -i). No python dependency.
	wrapper := filepath.Join(binDir, kind)
	script := fmt.Sprintf(`#!/bin/sh
# FAC-133 contained agent wrapper (deny-by-default seatbelt + env -i).
# Trusted worker-side re-verification runs BEFORE sandbox-exec (wrapper is
# not yet sandboxed). Sealed control lives under shared .herd (agent cannot
# rewrite under seatbelt). HERD_SEALED_CONTROL + HERD_CONTROL_SECRET must
# be present in ENVFILE for re-verify; missing seal fails closed when required.
set -e
PROFILE=%q
ENVFILE=%q
REAL=%q
# Optional sealed-control re-verify (coordinator-sealed path + MAC secret).
if [ -f "$ENVFILE" ]; then
  SEALED=""
  CSECRET=""
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      HERD_SEALED_CONTROL=*) SEALED="${line#HERD_SEALED_CONTROL=}" ;;
      HERD_CONTROL_SECRET=*) CSECRET="${line#HERD_CONTROL_SECRET=}" ;;
    esac
  done < "$ENVFILE"
  TASK=""
  LEASE=""
  WORKER=""
  WAIT=""
  while IFS= read -r line || [ -n "$line" ]; do
    case "$line" in
      HERD_SEALED_CONTROL=*) SEALED="${line#HERD_SEALED_CONTROL=}" ;;
      HERD_EXPECTED_TASK=*) TASK="${line#HERD_EXPECTED_TASK=}" ;;
      HERD_EXPECTED_LEASE=*) LEASE="${line#HERD_EXPECTED_LEASE=}" ;;
      HERD_EXPECTED_WORKER=*) WORKER="${line#HERD_EXPECTED_WORKER=}" ;;
      HERD_SEAL_WAIT=*) WAIT="${line#HERD_SEAL_WAIT=}" ;;
    esac
  done < "$ENVFILE"
  if [ -n "$SEALED" ]; then
    # Causal start barrier: wait for live-session seal before sandbox-exec.
    if [ "$WAIT" = "1" ]; then
      i=0
      while [ $i -lt 600 ]; do
        if [ -f "$SEALED" ]; then break; fi
        i=$((i+1)); sleep 0.05
      done
    fi
    if [ ! -f "$SEALED" ]; then
      echo "FAC-133: sealed control missing (start barrier): $SEALED" >&2
      exit 78
    fi
    # Re-read binding after wait so coordinator can set HERD_EXPECTED_WORKER
    # once the live AgentSessionID is known (post-start, pre-sandbox-exec).
    TASK=""; LEASE=""; WORKER=""
    while IFS= read -r line || [ -n "$line" ]; do
      case "$line" in
        HERD_EXPECTED_TASK=*) TASK="${line#HERD_EXPECTED_TASK=}" ;;
        HERD_EXPECTED_LEASE=*) LEASE="${line#HERD_EXPECTED_LEASE=}" ;;
        HERD_EXPECTED_WORKER=*) WORKER="${line#HERD_EXPECTED_WORKER=}" ;;
      esac
    done < "$ENVFILE"
    if [ -z "$TASK" ] || [ -z "$LEASE" ]; then
      echo "FAC-133: HERD_EXPECTED_TASK and HERD_EXPECTED_LEASE required" >&2
      exit 78
    fi
    if [ -z "$WORKER" ]; then
      echo "FAC-133: HERD_EXPECTED_WORKER (live AgentSessionID) required after start barrier" >&2
      exit 78
    fi
    case "$WORKER" in
      pending-*)
        echo "FAC-133: provisional worker refused: $WORKER" >&2
        exit 78
        ;;
    esac
    if command -v herd >/dev/null 2>&1; then
      herd control verify-sealed --file "$SEALED" --task "$TASK" --lease "$LEASE" --worker "$WORKER" || exit 78
    else
      echo "FAC-133: herd binary required for sealed control re-verify" >&2
      exit 78
    fi
  fi
fi
# Preserve agent argv before rebuilding "$@" from the env file.
n=$#
i=1
while [ "$i" -le "$n" ]; do
  eval "a_$i=\$$i"
  i=$((i + 1))
done
set --
while IFS= read -r line || [ -n "$line" ]; do
  case "$line" in
    "") continue ;;
    HERD_CONTROL_SECRET=*) continue ;; # never pass MAC secret into sandbox env
    *=*) set -- "$@" "$line" ;;
  esac
done < "$ENVFILE"
set -- "$@" "$REAL"
i=1
while [ "$i" -le "$n" ]; do
  eval "set -- \"\$@\" \"\$a_$i\""
  i=$((i + 1))
done
exec /usr/bin/sandbox-exec -f "$PROFILE" /usr/bin/env -i "$@"
`, profilePath, envFile, realAgentPath)
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		return "", "", "", err
	}
	return binDir, profilePath, envFile, nil
}

func (d DarwinSeatbelt) WrapCmd(cmd *exec.Cmd, profilePath string, scrubbedEnv []string) error {
	if cmd == nil {
		return fmt.Errorf("nil cmd")
	}
	if profilePath == "" {
		return ErrContainmentUnavailable
	}
	origPath := cmd.Path
	if origPath == "" && len(cmd.Args) > 0 {
		origPath = cmd.Args[0]
	}
	// sandbox-exec -f profile /usr/bin/env -i KEY=VAL... binary args...
	args := []string{"-f", profilePath, "/usr/bin/env", "-i"}
	args = append(args, scrubbedEnv...)
	args = append(args, origPath)
	if len(cmd.Args) > 1 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path = "/usr/bin/sandbox-exec"
	cmd.Args = append([]string{"sandbox-exec"}, args...)
	cmd.Env = scrubbedEnv
	return nil
}

// ProveDenials is hermetic: disposable fixture only; production worktree and
// shared checkout are never mutated. When shared is a git repo, status and
// worktree registry must remain byte-identical.
func (d DarwinSeatbelt) ProveDenials(worktree string, policy *LaunchPolicy, grant *LaunchGrant, profilePath string, scrubbedEnv []string) error {
	if !d.Available() {
		return ErrContainmentUnavailable
	}
	if policy == nil || grant == nil {
		return fmt.Errorf("%w: nil policy/grant", ErrUnknownPolicy)
	}
	absWT, err := realPath(worktree)
	if err != nil {
		return err
	}
	sharedAbs, _ := realPath(policy.SharedCheckout)
	beforeStatus, beforeWT := snapshotGitState(sharedAbs)

	probeRoot, err := os.MkdirTemp("", "fac133-probe-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(probeRoot)
	probeRoot, _ = realPath(probeRoot)

	fixtureShared := filepath.Join(probeRoot, "shared")
	fixtureWT := filepath.Join(fixtureShared, "wt")
	if err := os.MkdirAll(filepath.Join(fixtureWT, ".tmp"), 0o755); err != nil {
		return err
	}
	_ = os.WriteFile(filepath.Join(fixtureShared, "go.mod"), []byte("module fixture\n"), 0o644)

	fakeHome := filepath.Join(probeRoot, "home")
	fakeSSH := filepath.Join(fakeHome, ".ssh")
	fakeCfg := filepath.Join(fakeHome, ".config", "gcloud")
	sibling := filepath.Join(probeRoot, "sibling-repo")
	for _, d := range []string{fakeSSH, fakeCfg, sibling, filepath.Join(fakeHome, ".kube")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return fmt.Errorf("probe fixture mkdir: %w", err)
		}
	}
	sshKey := filepath.Join(fakeSSH, "id_rsa")
	kubeCfg := filepath.Join(fakeHome, ".kube", "config")
	gcloudCreds := filepath.Join(fakeCfg, "credentials.db")
	sibSecret := filepath.Join(sibling, "secret.go")
	for _, pair := range []struct{ path, body string }{
		{sshKey, "SSH_PRIVATE_KEY_PROBE"},
		{kubeCfg, "KUBE_CONFIG_PROBE"},
		{gcloudCreds, "GCLOUD_CREDS_PROBE"},
		{sibSecret, "SIBLING_REPO_SECRET"},
	} {
		if err := os.WriteFile(pair.path, []byte(pair.body), 0o600); err != nil {
			return fmt.Errorf("probe fixture write: %w", err)
		}
		// Non-vacuity: markers must exist for denial assertions.
		if b, err := os.ReadFile(pair.path); err != nil || !strings.Contains(string(b), pair.body) {
			return fmt.Errorf("probe fixture not readable before denial: %s", pair.path)
		}
	}

	netprobe, nerr := buildNetprobe(filepath.Join(fixtureWT, ".herd", "contain", "bin"))
	if nerr != nil {
		return fmt.Errorf("netprobe build: %w", nerr)
	}

	probePolicy := *policy
	probePolicy.FilesystemRoot = fixtureWT
	probePolicy.SharedCheckout = fixtureShared
	probeGrant := *grant
	probeGrant.CWD = fixtureWT
	_, fixtureProfile, _, ierr := d.Install(fixtureWT, &probePolicy, &probeGrant, "true", "/usr/bin/true")
	if ierr != nil {
		return ierr
	}
	if err := appendExecAllow(fixtureProfile, netprobe); err != nil {
		return err
	}
	// Child-observed env scrub needs env/sh on the process-exec allowlist.
	for _, bin := range []string{"/usr/bin/env", "/bin/sh", "/usr/bin/printenv"} {
		if _, err := os.Stat(bin); err == nil {
			if err := appendExecAllow(fixtureProfile, bin); err != nil {
				return err
			}
		}
	}

	// 1) Child-observed ambient credential scrub (non-vacuous).
	// Parent ambient canary must NOT appear in the contained child's environment.
	// Parent-slice inspection alone is vacuous; we execute a child that prints env.
	canary := "FAC133_CANARY_" + fmt.Sprintf("%d", time.Now().UnixNano())
	_ = os.Setenv(canary, "LEAKED_SECRET_VALUE")
	defer os.Unsetenv(canary)
	for _, e := range scrubbedEnv {
		if strings.Contains(e, "LEAKED_SECRET_VALUE") || strings.HasPrefix(e, canary+"=") {
			return fmt.Errorf("%w: scrubbed env slice leaked ambient canary", ErrContainmentProbeFailed)
		}
	}
	// Positive control: inject a known marker the child MUST see (proves env -i applied).
	childMarker := "HERD_SCRUB_PROBE=VISIBLE_" + fmt.Sprintf("%d", time.Now().UnixNano())
	probeEnv := append(append([]string{}, scrubbedEnv...), childMarker)
	// Child dumps env; ambient canary must be absent, probe marker must be present.
	// Prefer /usr/bin/env (always on exec allowlist via WrapCmd path); fall back to printenv.
	out, rerr := runContained(fixtureProfile, probeEnv, fixtureWT, "/usr/bin/env")
	if rerr != nil {
		// Some profiles deny bare env; use sh -c with env when shell-exec not granted.
		// /usr/bin/true alone is vacuous — require a child that observes variables.
		out, rerr = runContained(fixtureProfile, probeEnv, fixtureWT, "/bin/sh", "-c",
			`/usr/bin/env; if [ -n "$`+canary+`" ]; then echo LEAKED_VISIBLE; fi`)
	}
	if rerr != nil {
		return fmt.Errorf("%w: child-observed env scrub probe failed: %v out=%q", ErrContainmentProbeFailed, rerr, out)
	}
	if strings.Contains(out, "LEAKED_SECRET_VALUE") || strings.Contains(out, canary+"=") || strings.Contains(out, "LEAKED_VISIBLE") {
		return fmt.Errorf("%w: child observed ambient canary (env scrub failed)", ErrContainmentProbeFailed)
	}
	if !strings.Contains(out, "HERD_SCRUB_PROBE=VISIBLE_") {
		return fmt.Errorf("%w: child did not observe injected scrub marker (probe vacuous): %q", ErrContainmentProbeFailed, out)
	}

	// 2) Forbidden absolute path reads via /bin/cat.
	for _, f := range []struct{ path, marker string }{
		{sshKey, "SSH_PRIVATE_KEY_PROBE"},
		{kubeCfg, "KUBE_CONFIG_PROBE"},
		{gcloudCreds, "GCLOUD_CREDS_PROBE"},
		{sibSecret, "SIBLING_REPO_SECRET"},
	} {
		out, rerr = runContained(fixtureProfile, scrubbedEnv, fixtureWT, "/bin/cat", f.path)
		if strings.Contains(out, f.marker) {
			return fmt.Errorf("%w: child read forbidden path", ErrContainmentProbeFailed)
		}
		// Non-vacuous: sandbox must deny (error) — empty success is a fail.
		if rerr == nil {
			return fmt.Errorf("%w: forbidden read of %s returned success without marker", ErrContainmentProbeFailed, filepath.Base(f.path))
		}
	}

	// 3) Shared checkout file outside worktree.
	sharedFile := filepath.Join(fixtureShared, "go.mod")
	if b, err := os.ReadFile(sharedFile); err != nil || !strings.Contains(string(b), "module fixture") {
		return fmt.Errorf("shared fixture go.mod missing before denial probe")
	}
	if out, e2 := runContained(fixtureProfile, scrubbedEnv, fixtureWT, "/bin/cat", sharedFile); e2 == nil || strings.Contains(out, "module fixture") {
		return fmt.Errorf("%w: child read shared checkout file", ErrContainmentProbeFailed)
	}

	// 4) Outside write must fail (parent-side check after denied write attempt).
	outside := filepath.Join(probeRoot, "outside-write")
	if containsTool(grant.AllowedTools, "shell-exec") {
		_, _ = runContained(fixtureProfile, scrubbedEnv, fixtureWT, "/bin/sh", "-c", "echo pwn > "+shellQuote(outside))
	}
	if _, st := os.Stat(outside); st == nil {
		return fmt.Errorf("%w: child wrote outside worktree", ErrContainmentProbeFailed)
	}

	// 5) Inside allowed fixture read/write.
	inside := filepath.Join(fixtureWT, ".tmp", "inside-write")
	if containsTool(grant.AllowedTools, "shell-exec") {
		if _, err = runContained(fixtureProfile, scrubbedEnv, fixtureWT, "/bin/sh", "-c", "echo ok > "+shellQuote(inside)); err != nil {
			return fmt.Errorf("%w: child cannot write allowed fixture tmp: %v", ErrContainmentProbeFailed, err)
		}
	} else {
		if err := os.WriteFile(inside, []byte("ok\n"), 0o644); err != nil {
			return err
		}
		out, err = runContained(fixtureProfile, scrubbedEnv, fixtureWT, "/bin/cat", inside)
		if err != nil || !strings.Contains(out, "ok") {
			return fmt.Errorf("%w: child cannot read allowed fixture tmp: %v", ErrContainmentProbeFailed, err)
		}
	}

	// 6) Network: offline/limited must deny arbitrary egress via netprobe (allowed exec).
	netMode := strings.ToLower(grant.Network)
	if netMode == "offline" || netMode == "limited" {
		if _, err = runContained(fixtureProfile, scrubbedEnv, fixtureWT, netprobe, "1.1.1.1:443"); err == nil {
			return fmt.Errorf("%w: %s child reached arbitrary egress via netprobe", ErrContainmentProbeFailed, netMode)
		}
	}

	// 7) Forbidden tool on production profile + fixture.
	if _, st := os.Stat("/usr/bin/curl"); st == nil {
		if !containsTool(grant.AllowedTools, "curl") && !containsTool(grant.AllowedTools, "network") {
			if _, e2 := runContained(profilePath, scrubbedEnv, absWT, "/usr/bin/curl", "--help"); e2 == nil {
				return fmt.Errorf("%w: child executed forbidden tool curl", ErrContainmentProbeFailed)
			}
			if _, e2 := runContained(fixtureProfile, scrubbedEnv, fixtureWT, "/usr/bin/curl", "--help"); e2 == nil {
				return fmt.Errorf("%w: fixture child executed forbidden tool curl", ErrContainmentProbeFailed)
			}
		}
	}

	// 8) Exclusive package on fixture only.
	if policy.ExclusivePackages && len(policy.PackageAllowlist) > 0 {
		denyPkg := filepath.Join(fixtureWT, "pkg", "forbidden-probe")
		_ = os.MkdirAll(denyPkg, 0o755)
		denyFile := filepath.Join(denyPkg, "x.go")
		_ = os.WriteFile(denyFile, []byte("package forbidden_probe"), 0o644)
		// Rebuild fixture profile with exclusive packages against fixture paths.
		exPolicy := probePolicy
		exPolicy.ExclusivePackages = true
		exPolicy.PackageAllowlist = append([]string(nil), policy.PackageAllowlist...)
		_, exProfile, _, e3 := d.Install(fixtureWT, &exPolicy, &probeGrant, "true", "/usr/bin/true")
		if e3 != nil {
			return e3
		}
		out, _ = runContained(exProfile, scrubbedEnv, fixtureWT, "/bin/cat", denyFile)
		if strings.Contains(out, "package forbidden_probe") {
			return fmt.Errorf("%w: exclusive package allowlist not enforced", ErrContainmentProbeFailed)
		}
	}

	// 9) No production residue.
	for _, p := range []string{
		filepath.Join(absWT, ".tmp", ".fac133-inside-write"),
		filepath.Join(absWT, ".fac133-reviewer-write"),
		filepath.Join(absWT, "pkg", ".fac133-forbidden-probe"),
	} {
		if _, st := os.Stat(p); st == nil {
			return fmt.Errorf("%w: production worktree residue", ErrContainmentProbeFailed)
		}
	}

	afterStatus, afterWT := snapshotGitState(sharedAbs)
	// Empty beforeStatus is the normal clean working tree — still compare.
	if sharedAbs != "" && (beforeStatus != afterStatus || beforeWT != afterWT) {
		return fmt.Errorf("%w: git/worktree registry changed during probes", ErrContainmentProbeFailed)
	}
	return nil
}

func snapshotGitState(repo string) (status, worktrees string) {
	if repo == "" {
		return "", ""
	}
	// Probe git availability; non-repo returns empty pair (caller skips only if both empty AND no .git).
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		// Linked worktrees use .git file; still try git -C.
	}
	out, err := exec.Command("git", "-C", repo, "status", "--porcelain").CombinedOutput()
	if err != nil {
		return "<<not-a-git-repo>>", ""
	}
	status = string(out) // may be empty when clean
	out2, err2 := exec.Command("git", "-C", repo, "worktree", "list", "--porcelain").CombinedOutput()
	if err2 != nil {
		return status, "<<worktree-list-failed>>"
	}
	return status, string(out2)
}

func buildNetprobe(binDir string) (string, error) {
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", err
	}
	src := filepath.Join(binDir, "netprobe.go")
	dst := filepath.Join(binDir, "fac133-netprobe")
	code := "package main\nimport (\"net\"; \"os\"; \"time\")\nfunc main() {\n if len(os.Args) < 2 { os.Exit(2) }\n d := net.Dialer{Timeout: time.Second}\n c, err := d.Dial(\"tcp\", os.Args[1])\n if err != nil { os.Exit(1) }\n _ = c.Close()\n os.Exit(0)\n}\n"
	if err := os.WriteFile(src, []byte(code), 0o644); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", dst, src)
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build netprobe: %v: %s", err, out)
	}
	return dst, nil
}

func brokerPortOnly(endpoint string) string {
	_, port, err := splitHostPortSafe(endpoint)
	if err != nil || port == "" {
		return "0"
	}
	return port
}

func appendExecAllow(profilePath, binary string) error {
	b, err := os.ReadFile(profilePath)
	if err != nil {
		return err
	}
	extra := fmt.Sprintf("\n(allow process-exec* (literal %q))\n(allow file-read* (literal %q))\n", binary, binary)
	return os.WriteFile(profilePath, append(b, []byte(extra)...), 0o644)
}

func containsTool(tools []string, name string) bool {
	for _, t := range tools {
		if strings.EqualFold(t, name) {
			return true
		}
	}
	return false
}

func runContained(profilePath string, scrubbedEnv []string, dir string, bin string, args ...string) (string, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	sb := DarwinSeatbelt{}
	if err := sb.WrapCmd(cmd, profilePath, scrubbedEnv); err != nil {
		return "", err
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// seatbeltProfileDenyDefault is deny-by-default with process-exec allowlist
// derived from AllowedTools + verification profile (not hardcoded shell),
// aggressive FS denials, and network mode offline|limited|online.
func seatbeltProfileDenyDefault(worktree, shared, agentBinary string, grant *LaunchGrant, policy *LaunchPolicy) string {
	var b strings.Builder
	b.WriteString("(version 1)\n")
	b.WriteString("(deny default)\n")
	b.WriteString("(allow process-fork)\n")
	b.WriteString("(allow process-info*)\n")
	b.WriteString("(allow signal (target self))\n")
	b.WriteString("(allow sysctl-read)\n")
	b.WriteString("(allow mach-lookup)\n")
	// file-read* is required for dyld/system viability on Darwin. Portable
	// isolation is enforced by subsequent denials (home secrets, shared
	// checkout, shared parent = sibling repos) then re-allow worktree only.
	// Do not rely on hard-coded home-directory name heuristics (FAC-133 admission).
	b.WriteString("(allow file-ioctl)\n")
	b.WriteString("(allow file-read*)\n")
	b.WriteString("(allow file-write-data (literal \"/dev/null\"))\n")
	b.WriteString("(allow file-read-data (literal \"/dev/null\") (literal \"/dev/urandom\") (literal \"/dev/random\"))\n")

	testCmd := ""
	if policy != nil {
		testCmd = policy.TestCommand
	}
	for _, p := range ExecAllowlistForGrant(grant, agentBinary, testCmd) {
		if p == "" {
			continue
		}
		if isDir(p) {
			fmt.Fprintf(&b, "(allow process-exec* (subpath %q))\n", p)
			continue
		}
		fmt.Fprintf(&b, "(allow process-exec* (literal %q))\n", p)
	}
	if agentBinary != "" {
		fmt.Fprintf(&b, "(allow process-exec* (literal %q))\n", agentBinary)
	}
	containDir := filepath.Join(worktree, ".herd", "contain")
	fmt.Fprintf(&b, "(allow process-exec* (subpath %q))\n", containDir)

	// Network: offline deny; limited = loopback (+ explicit hosts); online allow.
	netMode := "offline"
	if grant != nil {
		netMode = strings.ToLower(grant.Network)
	}
	switch netMode {
	case "online":
		b.WriteString("(allow network*)\n")
	case "limited":
		// OS allows ONLY the durable broker endpoint (exact localhost:port).
		// Arbitrary localhost services must remain denied (localhost canary).
		// CONNECT destinations are enforced by the broker allowlist.
		b.WriteString("(deny network*)\n")
		if policy != nil && policy.BrokerEndpoint != "" {
			// macOS remote ip host must be "localhost" or "*"; pin exact broker port.
			port := brokerPortOnly(policy.BrokerEndpoint)
			if port != "" && port != "0" {
				fmt.Fprintf(&b, "(allow network-outbound (remote ip %q))\n", "localhost:"+port)
			}
		}
		// No generic localhost:* — non-broker loopback services stay denied.
	default:
		b.WriteString("(deny network*)\n")
	}

	// Aggressive denials then re-allow worktree.
	if home, err := os.UserHomeDir(); err == nil {
		home, _ = realPath(home)
		for _, p := range []string{
			filepath.Join(home, ".ssh"),
			filepath.Join(home, ".gnupg"),
			filepath.Join(home, ".aws"),
			filepath.Join(home, ".config"),
			filepath.Join(home, ".kube"),
			filepath.Join(home, ".docker"),
			filepath.Join(home, ".netrc"),
			filepath.Join(home, ".git-credentials"),
			home,
		} {
			fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", p)
			fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", p)
		}
	}
	for _, p := range []string{"/Users", "/tmp", "/private/tmp", "/private/var/folders", "/Volumes", "/var/folders"} {
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", p)
	}
	if shared != "" && shared != worktree {
		if sharedReal, err := realPath(shared); err == nil {
			shared = sharedReal
		}
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", shared)
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", shared)
		// Sibling repos under the same parent as shared (portable: any layout).
		// realPath required — sandbox denials match resolved paths only.
		if parent := filepath.Dir(shared); parent != "" && parent != "/" && parent != "." {
			if parentReal, err := realPath(parent); err == nil {
				parent = parentReal
			}
			fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", parent)
			fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", parent)
		}
	}
	// Extra deny roots: portable isolation beyond shared parent.
	// HERD_DENY_REPO_ROOTS=path:path allows operator-supplied secret/repo roots.
	// Also deny grandparent of shared (second-degree siblings under other trees).
	if shared != "" {
		if gp := filepath.Dir(filepath.Dir(shared)); gp != "" && gp != "/" && gp != "." {
			if gpReal, err := realPath(gp); err == nil {
				fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", gpReal)
				fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", gpReal)
			}
		}
	}
	if policy != nil {
		// RepoAllowlist identities never expand FS rights outside worktree.
		// Deny any absolute path listed in AmbientDenyRoots / env.
		for _, extra := range extraDenyRoots(policy) {
			if extra == "" {
				continue
			}
			if r, err := realPath(extra); err == nil {
				extra = r
			}
			// Never deny the worktree itself.
			if worktree != "" && (extra == worktree || strings.HasPrefix(worktree, extra+string(filepath.Separator))) {
				continue
			}
			fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", extra)
			fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", extra)
		}
		_ = policy.RepoAllowlist
	}

	if worktreeReal, err := realPath(worktree); err == nil {
		worktree = worktreeReal
	}
	// Worktree-rooted allow: re-allow only after all denials (last match wins for read).
	fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", worktree)
	if grant != nil && strings.EqualFold(grant.Role, RoleReviewer) {
		tmp := filepath.Join(worktree, ".tmp")
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", tmp)
	} else {
		fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", worktree)
	}

	if policy != nil && policy.ExclusivePackages && len(policy.PackageAllowlist) > 0 {
		pkgRoot := filepath.Join(worktree, "pkg")
		fmt.Fprintf(&b, "(deny file-read* (subpath %q))\n", pkgRoot)
		fmt.Fprintf(&b, "(deny file-write* (subpath %q))\n", pkgRoot)
		for _, pk := range policy.PackageAllowlist {
			pk = strings.Trim(pk, "/")
			full := filepath.Join(worktree, pk)
			fmt.Fprintf(&b, "(allow file-read* (subpath %q))\n", full)
			if grant == nil || !strings.EqualFold(grant.Role, RoleReviewer) {
				fmt.Fprintf(&b, "(allow file-write* (subpath %q))\n", full)
			}
		}
	}
	return b.String()
}

// extraDenyRoots collects absolute paths that must remain unreadable:
// HERD_DENY_REPO_ROOTS env (colon-separated) plus policy AmbientDeny if present.
func extraDenyRoots(policy *LaunchPolicy) []string {
	var out []string
	if raw := strings.TrimSpace(os.Getenv("HERD_DENY_REPO_ROOTS")); raw != "" {
		for _, p := range strings.Split(raw, ":") {
			p = strings.TrimSpace(p)
			if p != "" {
				out = append(out, p)
			}
		}
	}
	// Only operator-supplied absolute roots — do not deny /opt (Homebrew binaries).
	_ = policy
	return out
}

func toolExecPaths(tool string) []string {
	switch strings.ToLower(tool) {
	case "git-read", "git-write":
		return resolveBin("git")
	case "shell-exec":
		// Only when explicitly granted — not hardcoded for every role.
		return []string{"/bin/bash", "/bin/sh", "/usr/bin/env"}
	case "read-file", "write-file", "grep":
		return []string{"/bin/cat", "/usr/bin/grep", "/bin/ls", "/usr/bin/head", "/usr/bin/tail"}
	case "herd-verify", "herd-verify-read":
		return nil // verification binaries come from VerificationExecutables
	case "curl", "network":
		return resolveBin("curl")
	default:
		return nil
	}
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func realPath(p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved, nil
	}
	return abs, nil
}

// UpsertEnvFileKeys merges keys into an existing KEY=VALUE env file (or creates
// it). Used after live AgentSessionID resolve to publish HERD_EXPECTED_WORKER
// for the start-barrier wrapper re-read.
func UpsertEnvFileKeys(path string, keys map[string]string) error {
	if path == "" || len(keys) == 0 {
		return fmt.Errorf("env path and keys required")
	}
	existing := map[string]string{}
	order := []string{}
	if b, err := os.ReadFile(path); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			line = strings.TrimRight(line, "\r")
			if line == "" || !strings.Contains(line, "=") {
				continue
			}
			k, v, _ := strings.Cut(line, "=")
			if _, seen := existing[k]; !seen {
				order = append(order, k)
			}
			existing[k] = v
		}
	}
	for k, v := range keys {
		if k == "" || strings.ContainsAny(k, "=\n\r\x00") || strings.ContainsAny(v, "\n\r\x00") {
			return fmt.Errorf("invalid env key/value")
		}
		if _, seen := existing[k]; !seen {
			order = append(order, k)
		}
		existing[k] = v
	}
	out := make([]string, 0, len(order))
	for _, k := range order {
		out = append(out, k+"="+existing[k])
	}
	return WriteEnvFile(path, out)
}

// WriteEnvFile writes KEY=VALUE lines (one per line; values may contain spaces).
// Newlines and NULs inside entries are rejected.
func WriteEnvFile(path string, env []string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var b strings.Builder
	for _, e := range env {
		if e == "" || !strings.Contains(e, "=") {
			continue
		}
		if strings.ContainsAny(e, "\n\x00") {
			return fmt.Errorf("security: env entry contains NUL/newline")
		}
		b.WriteString(e)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// ResolveAgentBinary finds the real harness binary for kind.
func ResolveAgentBinary(kind string) (string, error) {
	candidates := []string{kind}
	switch strings.ToLower(kind) {
	case "opencode":
		candidates = []string{"opencode"}
	case "claude":
		candidates = []string{"claude"}
	case "codex":
		candidates = []string{"codex"}
	case "grok":
		candidates = []string{"grok"}
	case "agy", "antigravity":
		candidates = []string{"agy", "antigravity"}
	case "true":
		candidates = []string{"true", "/usr/bin/true", "/bin/true"}
	}
	for _, c := range candidates {
		if p, err := exec.LookPath(c); err == nil {
			return p, nil
		}
		if strings.HasPrefix(c, "/") {
			if _, err := os.Stat(c); err == nil {
				return c, nil
			}
		}
	}
	return "", fmt.Errorf("agent binary for kind %q not found in PATH", kind)
}
