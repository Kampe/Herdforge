// Package contextauth defines the shared TaskContext model, verification rules,
// and filename constants for launch receipts across packages without import cycles.
package contextauth

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// TaskContextFile is the launch receipt written into every isolated agent worktree.
const TaskContextFile = gitroot.TaskContextFile

// ReceiptPubFile is the published verification key, relative to repo root.
const ReceiptPubFile = ".herd/receipt.pub"

// CanonicalTaskContextDir is the coordinator's DURABLE task-context store,
// relative to the repo root.
const CanonicalTaskContextDir = ".herd/task-context-receipts"

// TaskContext binds an isolated agent to its repository, task provider,
// project, task ref, candidate/base commits, lease generation, role,
// allowed operations, and expiry.
type TaskContext struct {
	ProviderType      string    `json:"provider_type"`
	ProjectID         string    `json:"project_id"`
	ProviderWorkspace string    `json:"provider_workspace,omitempty"`
	ProviderProfile   string    `json:"provider_profile,omitempty"`
	Repository        string    `json:"repository"`
	Role              string    `json:"role"`
	TaskRef           string    `json:"task_ref"`
	TaskID            string    `json:"task_id"`
	Branch            string    `json:"branch"`
	BaseSHA           string    `json:"base_sha"`
	CandidateSHA      string    `json:"candidate_sha,omitempty"`
	AuthorityScope    string    `json:"authority_scope,omitempty"`
	AnchorRef         string    `json:"anchor_ref,omitempty"`
	HerdrWorkspace    string    `json:"herdr_workspace,omitempty"`
	LeaseID           string    `json:"lease_id"`
	LeaseGeneration   int64     `json:"lease_generation"`
	LeaseTaskRef      string    `json:"lease_task_ref"`
	SessionID         string    `json:"session_id"`
	AgentSessionID    string    `json:"agent_session_id,omitempty"`
	AllowedOps        []string  `json:"allowed_ops"`
	ExpiresAt         time.Time `json:"expires_at"`
	Signature         string    `json:"signature,omitempty"`
}

// CanonicalBytes returns the canonical serialized bytes of the task context for signing/verification.
func CanonicalBytes(tc TaskContext) ([]byte, error) {
	tc.Signature = ""
	data, err := json.Marshal(tc)
	if err != nil {
		return nil, fmt.Errorf("canonicalize receipt: %w", err)
	}
	return data, nil
}

// ReadTaskContext loads the task context from worktreePath/TASK-CONTEXT.json without verifying signature.
func ReadTaskContext(worktreePath string) (TaskContext, error) {
	var tc TaskContext
	data, err := os.ReadFile(filepath.Join(worktreePath, TaskContextFile))
	if err != nil {
		return tc, fmt.Errorf("read %s: %w", TaskContextFile, err)
	}
	if err := json.Unmarshal(data, &tc); err != nil {
		return tc, fmt.Errorf("unmarshal %s: %w", TaskContextFile, err)
	}
	return tc, nil
}

// LoadReceiptPublicKey loads the Ed25519 public key from repoRoot/.herd/receipt.pub.
func LoadReceiptPublicKey(repoRoot string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ReceiptPubFile))
	if err != nil {
		return nil, fmt.Errorf("no receipt verification key at %s: %w", filepath.Join(repoRoot, ReceiptPubFile), err)
	}
	raw, decErr := hex.DecodeString(strings.TrimSpace(string(data)))
	if decErr != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("receipt verification key is corrupt")
	}
	return ed25519.PublicKey(raw), nil
}

// VerifyTaskContext verifies the cryptographic signature on a TaskContext against the public key.
func VerifyTaskContext(pub ed25519.PublicKey, tc TaskContext) error {
	if len(pub) != ed25519.PublicKeySize {
		return errors.New("invalid verification key")
	}
	sigHex := strings.TrimSpace(tc.Signature)
	if sigHex == "" {
		return fmt.Errorf("receipt for %s is unsigned", tc.TaskRef)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("receipt for %s carries malformed signature", tc.TaskRef)
	}
	canonical, err := CanonicalBytes(tc)
	if err != nil {
		return err
	}
	if !ed25519.Verify(pub, canonical, sig) {
		return fmt.Errorf("receipt for %s failed signature verification", tc.TaskRef)
	}
	return nil
}

// ReadAndVerifyTaskContext reads worktreePath/TASK-CONTEXT.json and verifies it against repoRoot/.herd/receipt.pub.
func ReadAndVerifyTaskContext(repoRoot, worktreePath string) (TaskContext, error) {
	tc, err := ReadTaskContext(worktreePath)
	if err != nil {
		return tc, err
	}
	pub, err := LoadReceiptPublicKey(repoRoot)
	if err != nil {
		return tc, err
	}
	if err := VerifyTaskContext(pub, tc); err != nil {
		return tc, err
	}
	return tc, nil
}
