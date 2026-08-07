package confinement

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Enforcer is the production gate used by dispatch before a write-capable
// agent process starts. Tests inject FakeOS; production uses RequireOS.
type Enforcer struct {
	Issuer Issuer
	OS     OSBackend
	// SkipOS is test-only. Production must leave it false.
	SkipOS bool
	// ReceiptDir, when set, persists binding receipts as JSONL.
	ReceiptDir string

	mu sync.Mutex
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

// BindAndProve authenticates the worktree, issues a MAC capability, proves OS
// write denials for the incident-shaped paths, and returns a durable binding.
func (e *Enforcer) BindAndProve(id LaunchIdentity) (*Binding, error) {
	if e == nil || e.Issuer == nil {
		return nil, ErrUnauthenticated
	}
	binding, err := Bind(id, e.Issuer)
	if err != nil {
		return nil, err
	}
	// Policy layer must accept in-worktree and deny shared-root absolute path.
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
		osb := e.OS
		if osb == nil {
			var err error
			osb, err = RequireOS()
			if err != nil {
				return nil, err
			}
		}
		if err := osb.ProveWriteDenials(binding.WorktreeRoot, binding.SharedRoot); err != nil {
			return nil, err
		}
		binding.OSBackend = osb.Name()
		binding.OSProved = true
	}
	if err := binding.CheckSharedRoot(); err != nil {
		return nil, err
	}
	binding.ReceiptDigest = binding.digest()
	if err := e.persist(binding); err != nil {
		return nil, err
	}
	return binding, nil
}

func (e *Enforcer) persist(b *Binding) error {
	if e == nil || strings.TrimSpace(e.ReceiptDir) == "" || b == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := os.MkdirAll(e.ReceiptDir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(e.ReceiptDir, "confinement-receipts.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	type line struct {
		CreatedAt      time.Time `json:"created_at"`
		Task           string    `json:"task"`
		Worktree       string    `json:"worktree"`
		SharedRoot     string    `json:"shared_root,omitempty"`
		ReceiptDigest  string    `json:"receipt_digest"`
		OSBackend      string    `json:"os_backend,omitempty"`
		OSProved       bool      `json:"os_proved"`
		PolicyDigest   string    `json:"policy_digest"`
		ProofNonce     string    `json:"proof_nonce"`
	}
	payload, err := json.Marshal(line{
		CreatedAt:     b.CreatedAt,
		Task:          b.Tuple.Task,
		Worktree:      b.WorktreeRoot,
		SharedRoot:    b.SharedRoot,
		ReceiptDigest: b.ReceiptDigest,
		OSBackend:     b.OSBackend,
		OSProved:      b.OSProved,
		PolicyDigest:  b.PolicyDigest,
		ProofNonce:    b.ProofNonce,
	})
	if err != nil {
		return err
	}
	if _, err := f.Write(append(payload, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
