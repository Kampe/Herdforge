// Package agentpolicy defines the fleet-only nested-agent boundary.
//
// The HMAC in Contract is metadata integrity for handoff/evidence. It is not
// an OS or tool enforcement boundary; only compiled harness controls can
// prevent a child tool from being exposed.
package agentpolicy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
)

type ChildKind string

const (
	ChildClaudeAgent        ChildKind = "claude-agent"
	ChildClaudeTask         ChildKind = "claude-task"
	ChildCodexSubagent      ChildKind = "codex-subagent"
	ChildCodexCollaboration ChildKind = "codex-collaboration"
	ChildRecovery           ChildKind = "recovery-resume"
	ChildReviewer           ChildKind = "reviewer"
	ChildVerifier           ChildKind = "verifier"
	ChildWorker             ChildKind = "worker"
	ChildCoordinator        ChildKind = "coordinator"
	ChildExternalRepository ChildKind = "external-repository"
)

type Operation string

const (
	OperationNestedAgent   Operation = "nested-agent"
	OperationShell         Operation = "shell"
	OperationHerdrDispatch Operation = "herdr-dispatch"
)

type Contract struct {
	Repository            string `json:"repository"`
	Task                  string `json:"task"`
	Lane                  string `json:"lane"`
	Role                  string `json:"role"`
	LeaseGeneration       int64  `json:"lease_generation"`
	HerdrSession          string `json:"herdr_session"`
	HerdrTab              string `json:"herdr_tab"`
	HerdrPane             string `json:"herdr_pane"`
	ParentExecutionFamily string `json:"parent_execution_family"`
	AllowedHerdrSurface   string `json:"allowed_herdr_surface"`
	PolicyDigest          string `json:"policy_digest"`
	AuthTag               string `json:"auth_tag"`
}

var (
	ErrInvalidContract = errors.New("agentpolicy: invalid or unauthenticated contract")
	ErrDenied          = errors.New("agentpolicy: nested agent denied")
	ErrEvidence        = errors.New("agentpolicy: invalid denial evidence")
)

func NewContract(repository, task, lane, role string, generation int64, session, tab, pane, family, surface string, key []byte) (Contract, error) {
	if len(key) == 0 {
		return Contract{}, errors.New("metadata key must be nonempty")
	}
	c := Contract{Repository: normalize(repository), Task: normalize(task), Lane: normalize(lane), Role: normalize(role), LeaseGeneration: generation, HerdrSession: normalize(session), HerdrTab: normalize(tab), HerdrPane: normalize(pane), ParentExecutionFamily: normalize(family), AllowedHerdrSurface: normalize(surface)}
	if err := c.validFields(false); err != nil {
		return Contract{}, err
	}
	c.PolicyDigest = c.digest()
	c.AuthTag = authenticate(c, key)
	return c, nil
}

func (c Contract) Verify(key []byte) error {
	if len(key) == 0 {
		return ErrInvalidContract
	}
	if err := c.validFields(true); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidContract, err)
	}
	if c.PolicyDigest != c.digest() || !hmac.Equal([]byte(c.AuthTag), []byte(authenticate(c, key))) {
		return ErrInvalidContract
	}
	return nil
}

func (c Contract) validFields(authenticated bool) error {
	for name, value := range map[string]string{"repository": c.Repository, "task": c.Task, "lane": c.Lane, "role": c.Role, "herdr_session": c.HerdrSession, "herdr_tab": c.HerdrTab, "herdr_pane": c.HerdrPane, "parent_execution_family": c.ParentExecutionFamily, "allowed_herdr_surface": c.AllowedHerdrSurface} {
		if value == "" || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s is required", name)
		}
	}
	if c.LeaseGeneration < 1 {
		return errors.New("lease generation must be positive")
	}
	if authenticated && (len(c.AuthTag) == 0 || len(c.PolicyDigest) == 0) {
		return errors.New("policy authentication is required")
	}
	return nil
}

func normalize(value string) string { return strings.ToLower(strings.TrimSpace(value)) }

func (c Contract) digest() string {
	b, _ := json.Marshal(struct {
		Repository, Task, Lane, Role                                                  string
		LeaseGeneration                                                               int64
		HerdrSession, HerdrTab, HerdrPane, ParentExecutionFamily, AllowedHerdrSurface string
	}{c.Repository, c.Task, c.Lane, c.Role, c.LeaseGeneration, c.HerdrSession, c.HerdrTab, c.HerdrPane, c.ParentExecutionFamily, c.AllowedHerdrSurface})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func authenticate(c Contract, key []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(c.digest()))
	return hex.EncodeToString(mac.Sum(nil))
}

type Attempt struct {
	Operation  Operation
	Child      ChildKind
	Repository string
	Surface    string
	Family     string
}

func (c Contract) Decide(key []byte, attempt Attempt) error {
	if err := c.Verify(key); err != nil {
		return err
	}
	if normalize(attempt.Repository) != c.Repository || normalize(attempt.Surface) != c.AllowedHerdrSurface || normalize(attempt.Family) != c.ParentExecutionFamily {
		return ErrDenied
	}
	if attempt.Operation == OperationShell || attempt.Operation == OperationHerdrDispatch {
		return nil
	}
	if attempt.Operation == OperationNestedAgent {
		return ErrDenied
	}
	return ErrDenied
}

type DenialEvidence struct {
	Repository       string    `json:"repository"`
	Task             string    `json:"task"`
	Lane             string    `json:"lane"`
	Role             string    `json:"role"`
	HerdrSession     string    `json:"herdr_session"`
	HerdrTab         string    `json:"herdr_tab"`
	HerdrPane        string    `json:"herdr_pane"`
	LeaseGeneration  int64     `json:"lease_generation"`
	Sequence         int64     `json:"sequence"`
	Child            ChildKind `json:"child"`
	Reason           string    `json:"reason,omitempty"`
	ContractDigest   string    `json:"policy_digest"`
	Operation        Operation `json:"operation"`
	AttemptedRepo    string    `json:"attempted_repository"`
	AttemptedSurface string    `json:"attempted_surface"`
	AttemptedFamily  string    `json:"attempted_family"`
}

type EvidenceStore struct {
	mu   sync.Mutex
	file *os.File
	next int64
}

func NewEvidenceStore(path string) (*EvidenceStore, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	s := &EvidenceStore{file: f}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	if err := s.readbackLocked(); err != nil {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
		return nil, err
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return s, nil
}
func (s *EvidenceStore) Close() error { return s.file.Close() }
func (s *EvidenceStore) Append(c Contract, key []byte, attempt Attempt, reason error) (DenialEvidence, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := syscall.Flock(int(s.file.Fd()), syscall.LOCK_EX); err != nil {
		return DenialEvidence{}, err
	}
	defer syscall.Flock(int(s.file.Fd()), syscall.LOCK_UN)
	if err := s.readbackLocked(); err != nil {
		return DenialEvidence{}, err
	}
	if err := c.Verify(key); err != nil {
		return DenialEvidence{}, err
	}
	decision := c.Decide(key, attempt)
	if !errors.Is(decision, ErrDenied) || !errors.Is(reason, ErrDenied) {
		return DenialEvidence{}, ErrEvidence
	}
	s.next++
	e := DenialEvidence{Repository: c.Repository, Task: c.Task, Lane: c.Lane, Role: c.Role, HerdrSession: c.HerdrSession, HerdrTab: c.HerdrTab, HerdrPane: c.HerdrPane, LeaseGeneration: c.LeaseGeneration, Sequence: s.next, Child: attempt.Child, Reason: decision.Error(), ContractDigest: c.PolicyDigest, Operation: attempt.Operation, AttemptedRepo: normalize(attempt.Repository), AttemptedSurface: normalize(attempt.Surface), AttemptedFamily: normalize(attempt.Family)}
	b, err := json.Marshal(e)
	if err != nil {
		return DenialEvidence{}, err
	}
	b = append(b, '\n')
	if _, err = s.file.Write(b); err != nil {
		s.next--
		return DenialEvidence{}, err
	}
	if err = s.file.Sync(); err != nil {
		return DenialEvidence{}, err
	}
	if err = s.readbackLocked(); err != nil {
		return DenialEvidence{}, err
	}
	return e, nil
}
func (s *EvidenceStore) readbackLocked() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	dec := json.NewDecoder(s.file)
	var last DenialEvidence
	for {
		var e DenialEvidence
		err := dec.Decode(&e)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || e.Sequence != last.Sequence+1 || e.Sequence < 1 || e.Repository == "" || e.Task == "" || e.HerdrSession == "" || e.ContractDigest == "" || e.Child == "" || e.Operation == "" || e.AttemptedRepo == "" || e.AttemptedSurface == "" || e.AttemptedFamily == "" {
			return ErrEvidence
		}
		last = e
	}
	s.next = last.Sequence
	_, err := s.file.Seek(0, io.SeekEnd)
	return err
}
