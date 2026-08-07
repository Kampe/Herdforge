package confinement

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// receiptFileMu serializes receipt directory creates across Enforcer clones.
var receiptFileMu sync.Mutex

// Enforcer is the production gate used by dispatch before a write-capable
// agent process starts. Tests inject FakeOS; production uses RequireOS.
type Enforcer struct {
	Issuer Issuer
	OS     OSBackend
	// SkipOS is test-only. Production must leave it false.
	SkipOS bool
	// ReceiptDir, when set, persists binding receipts as JSON files.
	ReceiptDir string
}

// PreparedOS is the durable OS material that must be installed before the
// Herdr tab is created (so PATH can include the agent wrapper) and is then
// bound into the MAC'd receipt after tab/pane identity is known.
type PreparedOS struct {
	Backend     string
	ProfilePath string
	BinDir      string
	Kind        string
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

// PrepareOS installs the seatbelt profile, proves write denials under that
// exact profile, and installs the PATH-first agent wrapper. Call this before
// TabCreate so the pane inherits PATH with the wrapper first.
func (e *Enforcer) PrepareOS(worktree, sharedRoot, kind, realAgent string) (*PreparedOS, error) {
	if e == nil {
		return nil, ErrUnauthenticated
	}
	if e.SkipOS {
		return &PreparedOS{Backend: "skipped"}, nil
	}
	osb := e.OS
	if osb == nil {
		var err error
		osb, err = RequireOS()
		if err != nil {
			return nil, err
		}
	}
	kind = strings.TrimSpace(kind)
	if kind == "" {
		return nil, fmt.Errorf("confinement: agent kind required")
	}
	profile, err := osb.Prepare(worktree, sharedRoot)
	if err != nil {
		return nil, err
	}
	if err := osb.ProveWriteDenials(worktree, sharedRoot, profile); err != nil {
		return nil, err
	}
	binDir, err := osb.InstallAgentWrapper(worktree, profile, kind, realAgent)
	if err != nil {
		return nil, fmt.Errorf("confinement: agent wrapper: %w", err)
	}
	probe := exec.Command("/usr/bin/true")
	if err := osb.Wrap(probe, profile); err != nil {
		return nil, fmt.Errorf("confinement: wrap self-check: %w", err)
	}
	return &PreparedOS{
		Backend:     osb.Name(),
		ProfilePath: profile,
		BinDir:      binDir,
		Kind:        kind,
	}, nil
}

// PathEnv returns KEY=VALUE for Herdr tab create --env so the agent wrapper
// is first on PATH for kind resolution.
func (p *PreparedOS) PathEnv(existing string) string {
	if p == nil || p.BinDir == "" {
		return ""
	}
	path := p.BinDir
	if existing != "" {
		path = p.BinDir + string(os.PathListSeparator) + existing
	}
	return "PATH=" + path
}

// BindAndProve authenticates the worktree, issues a MAC capability, and
// attaches a previously PreparedOS proof. Production requires prep with
// AgentWrapped material; SkipOS allows policy-only tests.
func (e *Enforcer) BindAndProve(id LaunchIdentity, prep *PreparedOS) (*Binding, error) {
	if e == nil || e.Issuer == nil {
		return nil, ErrUnauthenticated
	}
	if len(id.Argv) == 0 {
		return nil, fmt.Errorf("%w: launch argv is required", ErrUnauthenticated)
	}
	binding, err := Bind(id, e.Issuer)
	if err != nil {
		return nil, err
	}
	if err := binding.AuthorizeRelativeWrite(filepath.Join(".herd", "confine-ok")); err != nil {
		return nil, fmt.Errorf("confinement: policy self-check: %w", err)
	}
	if id.SharedRoot != "" {
		incident := filepath.Join(id.SharedRoot, ".herd", "FAC-188-R2-RESIDUAL.md")
		if err := binding.Boundary.AuthorizeWrite(binding.Capability, incident); err == nil {
			return nil, fmt.Errorf("%w: policy accepted shared-root incident path", ErrOutsideRoot)
		}
	}
	if !e.SkipOS {
		if prep == nil || prep.ProfilePath == "" || prep.BinDir == "" || prep.Backend == "" {
			return nil, fmt.Errorf("confinement: PreparedOS required before bind (call PrepareOS before TabCreate)")
		}
		// Re-prove with the exact installed profile so a swapped profile cannot
		// be bound after PATH was injected at tab create.
		osb := e.OS
		if osb == nil {
			var err error
			osb, err = RequireOS()
			if err != nil {
				return nil, err
			}
		}
		if err := osb.ProveWriteDenials(binding.WorktreeRoot, binding.SharedRoot, prep.ProfilePath); err != nil {
			return nil, fmt.Errorf("confinement: re-prove after tab: %w", err)
		}
		binding.OSBackend = prep.Backend
		binding.OSProved = true
		binding.AgentWrapped = true
		binding.ProfilePath = prep.ProfilePath
		binding.WrapperBinDir = prep.BinDir
	}
	if err := binding.CheckSharedRoot(); err != nil {
		return nil, err
	}
	if !e.SkipOS && (!binding.OSProved || !binding.AgentWrapped || binding.WrapperBinDir == "") {
		return nil, fmt.Errorf("confinement: agent wrap incomplete after OS proof")
	}
	digest, err := binding.digest()
	if err != nil {
		return nil, err
	}
	binding.ReceiptDigest = digest
	if err := e.persist(binding); err != nil {
		return nil, err
	}
	return binding, nil
}

func (e *Enforcer) persist(b *Binding) error {
	if e == nil || strings.TrimSpace(e.ReceiptDir) == "" || b == nil {
		return nil
	}
	receiptFileMu.Lock()
	defer receiptFileMu.Unlock()
	if err := os.MkdirAll(e.ReceiptDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(e.ReceiptDir, fmt.Sprintf("receipt-%s-%d.json", b.ProofNonce, b.CreatedAt.UnixNano()))
	type line struct {
		CreatedAt     time.Time `json:"created_at"`
		Task          string    `json:"task"`
		Worktree      string    `json:"worktree"`
		SharedRoot    string    `json:"shared_root,omitempty"`
		ReceiptDigest string    `json:"receipt_digest"`
		OSBackend     string    `json:"os_backend,omitempty"`
		OSProved      bool      `json:"os_proved"`
		AgentWrapped  bool      `json:"agent_wrapped"`
		ProfilePath   string    `json:"profile_path,omitempty"`
		WrapperBinDir string    `json:"wrapper_bin_dir,omitempty"`
		PolicyDigest  string    `json:"policy_digest"`
		ProofNonce    string    `json:"proof_nonce"`
	}
	payload, err := json.Marshal(line{
		CreatedAt:     b.CreatedAt,
		Task:          b.Tuple.Task,
		Worktree:      b.WorktreeRoot,
		SharedRoot:    b.SharedRoot,
		ReceiptDigest: b.ReceiptDigest,
		OSBackend:     b.OSBackend,
		OSProved:      b.OSProved,
		AgentWrapped:  b.AgentWrapped,
		ProfilePath:   b.ProfilePath,
		WrapperBinDir: b.WrapperBinDir,
		PolicyDigest:  b.PolicyDigest,
		ProofNonce:    b.ProofNonce,
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
