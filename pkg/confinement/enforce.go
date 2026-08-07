package confinement

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Enforcer is the production gate used by dispatch before a write-capable
// agent process starts. Tests inject FakeOS; production uses RequireOS.
type Enforcer struct {
	Issuer *HMACIssuer
	OS     OSBackend
	// ReceiptDir, when set, persists binding receipts as JSON files.
	// Prefer a path outside the agent write domain when possible; worktree
	// receipts remain HMAC-authenticated even if agent-writable.
	ReceiptDir string
}

// PreparedOS is the durable OS material installed before TabCreate.
// Profile and wrappers live in Session (outside the worktree write grant).
type PreparedOS struct {
	Backend       string
	ProfilePath   string
	ProfileDigest string
	BinDir        string
	Names         []string
	Session       SessionPaths
}

// ProductionEnforcer builds the fail-closed production enforcer from env.
func ProductionEnforcer() (*Enforcer, error) {
	issuer, err := IssuerFromEnv()
	if err != nil {
		return nil, err
	}
	osb, err := RequireOS()
	if err != nil {
		return nil, err
	}
	return &Enforcer{Issuer: issuer, OS: osb}, nil
}

// PrepareOS installs profile+wrappers into a coordinator-owned session
// directory outside the worktree, proves denials (including rewrite of the
// session profile), and freezes session modes.
func (e *Enforcer) PrepareOS(worktree, sharedRoot, taskRef string, leaseGeneration int64, branch, provider, realAgent string) (*PreparedOS, error) {
	if e == nil || e.Issuer == nil {
		return nil, ErrUnauthenticated
	}
	osb := e.OS
	if osb == nil {
		var err error
		osb, err = RequireOS()
		if err != nil {
			return nil, err
		}
	}
	names := WrapperNames(provider, realAgent)
	if len(names) == 0 {
		return nil, fmt.Errorf("confinement: agent wrapper names required (provider and/or argv0)")
	}
	session, err := NewSessionPaths(sharedRoot, taskRef, leaseGeneration)
	if err != nil {
		return nil, err
	}
	profile, err := osb.Prepare(worktree, sharedRoot, branch, session)
	if err != nil {
		return nil, err
	}
	digest, err := ProfileDigest(profile)
	if err != nil {
		return nil, err
	}
	if err := osb.ProveWriteDenials(worktree, sharedRoot, profile, session); err != nil {
		return nil, err
	}
	digest2, err := ProfileDigest(profile)
	if err != nil || digest2 != digest {
		return nil, fmt.Errorf("confinement: profile mutated during prove")
	}
	binDir, err := osb.InstallAgentWrappers(session, profile, names, realAgent)
	if err != nil {
		return nil, fmt.Errorf("confinement: agent wrapper: %w", err)
	}
	if err := VerifyAgentWrappers(binDir, profile, digest, names); err != nil {
		return nil, err
	}
	probe := exec.Command("/usr/bin/true")
	if err := osb.Wrap(probe, profile); err != nil {
		return nil, fmt.Errorf("confinement: wrap self-check: %w", err)
	}
	// Freeze after install so even a coordinator-side bug cannot leave
	// world-writable integrity material. Fail closed if freeze cannot apply.
	if err := FreezeSession(SessionPaths{Root: session.Root, Profile: profile, BinDir: binDir, ZdotDir: session.ZdotDir}); err != nil {
		return nil, fmt.Errorf("confinement: freeze session: %w", err)
	}
	return &PreparedOS{
		Backend:       osb.Name(),
		ProfilePath:   profile,
		ProfileDigest: digest,
		BinDir:        binDir,
		Names:         append([]string(nil), names...),
		Session:       session,
	}, nil
}

// TabEnv returns environment pairs for Herdr tab create.
// ZDOTDIR lives in the session directory (outside worktree), not under the
// agent write grant.
func (p *PreparedOS) TabEnv(worktree, existingPATH string) ([]string, error) {
	if p == nil || p.BinDir == "" || p.Session.ZdotDir == "" {
		return nil, fmt.Errorf("confinement: empty PreparedOS for TabEnv")
	}
	pathVal := p.BinDir
	if existingPATH != "" {
		pathVal = p.BinDir + string(os.PathListSeparator) + existingPATH
	}
	// TMPDIR remains under the worktree (agent must write temp files).
	tmpDir := filepath.Join(worktree, ".herd", "confine", "tmp")
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return nil, err
	}
	// Session zdot files may be 0444 from FreezeSession; thaw then rewrite.
	_ = os.Chmod(p.Session.ZdotDir, 0o755)
	rc := "# FAC-190 confinement zdot — keep wrapper first on PATH\n" +
		"export PATH=" + shellSingleArg(p.BinDir) + ":\"$PATH\"\n" +
		"export TMPDIR=" + shellSingleArg(tmpDir) + "\n" +
		"export TMP=" + shellSingleArg(tmpDir) + "\n" +
		"export TEMP=" + shellSingleArg(tmpDir) + "\n"
	for _, name := range []string{".zshrc", ".zprofile", ".zshenv"} {
		path := filepath.Join(p.Session.ZdotDir, name)
		if err := thawFile(path, 0o644); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(rc), 0o644); err != nil {
			return nil, err
		}
		// Re-freeze; agent still cannot write — session dir is outside worktree grant.
		_ = os.Chmod(path, 0o444)
	}
	return []string{
		"PATH=" + pathVal,
		"ZDOTDIR=" + p.Session.ZdotDir,
		"TMPDIR=" + tmpDir,
		"TMP=" + tmpDir,
		"TEMP=" + tmpDir,
		"HERD_CONFINEMENT_BIN=" + p.BinDir,
		"HERD_CONFINEMENT_PROFILE=" + p.ProfilePath,
		"HERD_CONFINEMENT_PROFILE_DIGEST=" + p.ProfileDigest,
	}, nil
}

// WrapperResolves reports whether name would resolve under BinDir.
func (p *PreparedOS) WrapperResolves(name string) bool {
	if p == nil || p.BinDir == "" || name == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(p.BinDir, filepath.Base(name)))
	return err == nil
}

// BindAndProve authenticates the worktree and attaches PreparedOS proof.
func (e *Enforcer) BindAndProve(id LaunchIdentity, prep *PreparedOS) (*Binding, error) {
	if e == nil || e.Issuer == nil {
		return nil, ErrUnauthenticated
	}
	if len(id.Argv) == 0 {
		return nil, fmt.Errorf("%w: launch argv is required", ErrUnauthenticated)
	}
	if prep == nil || prep.ProfilePath == "" || prep.ProfileDigest == "" || prep.BinDir == "" || prep.Backend == "" || len(prep.Names) == 0 {
		return nil, fmt.Errorf("confinement: PreparedOS required before bind (call PrepareOS before TabCreate)")
	}
	// Session integrity store must remain outside the worktree.
	if prep.Session.Root != "" {
		if isPathPrefix(prep.Session.Root, id.WorktreeRoot) || prep.Session.Root == id.WorktreeRoot {
			return nil, fmt.Errorf("confinement: session root inside worktree")
		}
	}
	binding, err := Bind(id, e.Issuer)
	if err != nil {
		return nil, err
	}
	if err := binding.AuthorizeRelativeWrite(filepath.Join(".herd", "confine-ok")); err != nil {
		return nil, fmt.Errorf("confinement: policy self-check: %w", err)
	}
	if id.SharedRoot != "" {
		incident := filepath.Join(id.SharedRoot, filepath.FromSlash(SharedRootIncidentRel))
		if err := binding.Boundary.AuthorizeWrite(binding.Capability, incident); err == nil {
			return nil, fmt.Errorf("%w: policy accepted shared-root incident path", ErrOutsideRoot)
		}
	}
	osb := e.OS
	if osb == nil {
		var err error
		osb, err = RequireOS()
		if err != nil {
			return nil, err
		}
	}
	gotDigest, err := ProfileDigest(prep.ProfilePath)
	if err != nil {
		return nil, err
	}
	if gotDigest != prep.ProfileDigest {
		return nil, fmt.Errorf("confinement: profile digest drift before re-prove")
	}
	if err := osb.ProveWriteDenials(binding.WorktreeRoot, binding.SharedRoot, prep.ProfilePath, prep.Session); err != nil {
		return nil, fmt.Errorf("confinement: re-prove after tab: %w", err)
	}
	gotDigest, err = ProfileDigest(prep.ProfilePath)
	if err != nil || gotDigest != prep.ProfileDigest {
		return nil, fmt.Errorf("confinement: profile digest drift after re-prove")
	}
	if err := VerifyAgentWrappers(prep.BinDir, prep.ProfilePath, prep.ProfileDigest, prep.Names); err != nil {
		return nil, fmt.Errorf("confinement: wrapper integrity after tab: %w", err)
	}
	binding.OSBackend = prep.Backend
	binding.OSProved = true
	// WrapperInstalled: session wrappers exist and pass VerifyAgentWrappers.
	// Does not claim herdr resolved the live agent through PATH (external CLI).
	binding.WrapperInstalled = true
	binding.ProfilePath = prep.ProfilePath
	binding.ProfileDigest = prep.ProfileDigest
	binding.WrapperBinDir = prep.BinDir
	binding.WrapperNames = append([]string(nil), prep.Names...)
	if err := binding.CheckSharedRoot(); err != nil {
		return nil, err
	}
	if !binding.OSProved || !binding.WrapperInstalled || binding.WrapperBinDir == "" || binding.ProfileDigest == "" {
		return nil, fmt.Errorf("confinement: wrapper install incomplete after OS proof")
	}
	if err := binding.SignReceipt(e.Issuer); err != nil {
		return nil, err
	}
	if err := e.persist(binding); err != nil {
		return nil, err
	}
	return binding, nil
}

func (e *Enforcer) persist(b *Binding) error {
	if e == nil || strings.TrimSpace(e.ReceiptDir) == "" || b == nil {
		return nil
	}
	if err := os.MkdirAll(e.ReceiptDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(e.ReceiptDir, fmt.Sprintf("receipt-%s-%d.json", b.ProofNonce, b.CreatedAt.UnixNano()))
	type line struct {
		CreatedAt        time.Time `json:"created_at"`
		Task             string    `json:"task"`
		Worktree         string    `json:"worktree"`
		SharedRoot       string    `json:"shared_root,omitempty"`
		ReceiptDigest    string    `json:"receipt_digest"`
		ReceiptMAC       string    `json:"receipt_mac"`
		OSBackend        string    `json:"os_backend,omitempty"`
		OSProved         bool      `json:"os_proved"`
		WrapperInstalled bool      `json:"wrapper_installed"`
		ProfilePath      string    `json:"profile_path,omitempty"`
		ProfileDigest    string    `json:"profile_digest,omitempty"`
		WrapperBinDir    string    `json:"wrapper_bin_dir,omitempty"`
		WrapperNames     []string  `json:"wrapper_names,omitempty"`
		PolicyDigest     string    `json:"policy_digest"`
		ProofNonce       string    `json:"proof_nonce"`
		ProofMAC         string    `json:"proof_mac"`
	}
	payload, err := json.Marshal(line{
		CreatedAt:        b.CreatedAt,
		Task:             b.Tuple.Task,
		Worktree:         b.WorktreeRoot,
		SharedRoot:       b.SharedRoot,
		ReceiptDigest:    b.ReceiptDigest,
		ReceiptMAC:       b.ReceiptMACHex,
		OSBackend:        b.OSBackend,
		OSProved:         b.OSProved,
		WrapperInstalled: b.WrapperInstalled,
		ProfilePath:      b.ProfilePath,
		ProfileDigest:    b.ProfileDigest,
		WrapperBinDir:    b.WrapperBinDir,
		WrapperNames:     b.WrapperNames,
		PolicyDigest:     b.PolicyDigest,
		ProofNonce:       b.ProofNonce,
		ProofMAC:         b.ProofMACHex,
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
