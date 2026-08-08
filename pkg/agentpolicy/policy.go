// Package agentpolicy defines the fleet-only nested-agent boundary.
//
// The HMAC in Contract is metadata integrity for handoff/evidence. It is not
// an OS or tool enforcement boundary; only compiled harness controls can
// prevent a child tool from being exposed.
//
// # Nested-agent policy is ADVISORY until FAC-205 lands
//
// What Herdforge actually enforces is its own refusal to launch: Validate
// runs before any tab or agent is created, and a decision that fails policy
// never reaches TabCreate or AgentStart. That refusal is real.
//
// What Herdforge does NOT enforce is the behaviour of an agent once it is
// running. A compiled denial (codex --disable multi_agent, claude
// --disallowed-tools Agent Task ToolSearch) is a flag asking a vendor CLI not
// to expose its nested-agent tools. Herdforge can prove the flag is in the
// argv it launched; it cannot prove the vendor honours it, and it cannot
// observe or stop a child that a running agent spawns anyway.
//
// Closing that gap needs herdr-side spawn supervision that durably returns
// child PID, start token, launch generation, and membership surviving
// double-fork/setns. That is FAC-205 and it is NOT landed. Until it lands, no
// caller may record a nested-agent denial as proven containment; the only
// provable claim is "this argv was refused" or "this argv carried the flag".
package agentpolicy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
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

const (
	SurfaceShell         = "shell"
	SurfaceHerdrDispatch = "herd dispatch"
	SurfaceHerdrSend     = "herd send"
	SurfaceHerdrReview   = "herd review"
	SurfaceNestedAgent   = "nested agent"
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
	ErrInvalidAttempt  = errors.New("agentpolicy: invalid attempted operation")
)

func NewContract(repository, task, lane, role string, generation int64, session, tab, pane, family, surface string, key []byte) (Contract, error) {
	if len(key) == 0 {
		return Contract{}, errors.New("metadata key must be nonempty")
	}
	c := Contract{Repository: normalizeRepository(repository), Task: normalizeOpaque(task), Lane: normalizeOpaque(lane), Role: normalizeEnum(role), LeaseGeneration: generation, HerdrSession: normalizeOpaque(session), HerdrTab: normalizeOpaque(tab), HerdrPane: normalizeOpaque(pane), ParentExecutionFamily: normalizeEnum(family), AllowedHerdrSurface: normalizeEnum(surface)}
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
	if !validHerdrSurface(c.AllowedHerdrSurface) {
		return errors.New("allowed Herdr surface is not a supported explicit surface")
	}
	if authenticated && (len(c.AuthTag) == 0 || len(c.PolicyDigest) == 0) {
		return errors.New("policy authentication is required")
	}
	return nil
}

func normalizeOpaque(value string) string { return strings.TrimSpace(value) }
func normalizeEnum(value string) string   { return strings.ToLower(strings.TrimSpace(value)) }

// normalizeRepository canonicalizes only the authoritative host component.
// Repository path components and local paths remain case-sensitive.
func normalizeRepository(value string) string {
	s := strings.TrimSpace(value)
	if strings.Contains(s, "://") {
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			u.Scheme, u.Host = strings.ToLower(u.Scheme), strings.ToLower(u.Host)
			return u.String()
		}
	}
	if slash := strings.IndexByte(s, '/'); slash > 0 {
		host := strings.ToLower(s[:slash])
		switch host {
		case "github.com", "gitlab.com", "bitbucket.org":
			return host + s[slash:]
		}
	}
	return s
}
func validHerdrSurface(value string) bool {
	return value == SurfaceHerdrDispatch || value == SurfaceHerdrSend || value == SurfaceHerdrReview
}

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
	if err := validateAttempt(attempt); err != nil {
		return err
	}
	if normalizeRepository(attempt.Repository) != c.Repository || normalizeEnum(attempt.Family) != c.ParentExecutionFamily {
		return ErrDenied
	}
	if attempt.Operation == OperationShell && normalizeEnum(attempt.Surface) == SurfaceShell {
		return nil
	}
	if attempt.Operation == OperationHerdrDispatch && normalizeEnum(attempt.Surface) == c.AllowedHerdrSurface {
		return nil
	}
	if attempt.Operation == OperationNestedAgent {
		return ErrDenied
	}
	return ErrDenied
}

func validateAttempt(a Attempt) error {
	if normalizeRepository(a.Repository) == "" || normalizeEnum(a.Surface) == "" || normalizeEnum(a.Family) == "" {
		return invalidAttempt()
	}
	switch a.Operation {
	case OperationShell:
		if normalizeEnum(a.Surface) != SurfaceShell || a.Child != "" {
			return invalidAttempt()
		}
	case OperationHerdrDispatch:
		if !validHerdrSurface(normalizeEnum(a.Surface)) || a.Child != "" {
			return invalidAttempt()
		}
	case OperationNestedAgent:
		if normalizeEnum(a.Surface) != SurfaceNestedAgent || !validChild(a.Child) {
			return invalidAttempt()
		}
	default:
		return invalidAttempt()
	}
	return nil
}

func invalidAttempt() error { return fmt.Errorf("%w: %w", ErrInvalidAttempt, ErrDenied) }

func validChild(child ChildKind) bool {
	switch child {
	case ChildClaudeAgent, ChildClaudeTask, ChildCodexSubagent, ChildCodexCollaboration, ChildRecovery, ChildReviewer, ChildVerifier, ChildWorker, ChildCoordinator, ChildExternalRepository:
		return true
	default:
		return false
	}
}

type DenialEvidence struct {
	Repository            string    `json:"repository"`
	Task                  string    `json:"task"`
	Lane                  string    `json:"lane"`
	Role                  string    `json:"role"`
	HerdrSession          string    `json:"herdr_session"`
	HerdrTab              string    `json:"herdr_tab"`
	HerdrPane             string    `json:"herdr_pane"`
	LeaseGeneration       int64     `json:"lease_generation"`
	Sequence              int64     `json:"sequence"`
	Child                 ChildKind `json:"child"`
	Reason                string    `json:"reason,omitempty"`
	ContractDigest        string    `json:"policy_digest"`
	Operation             Operation `json:"operation"`
	AttemptedRepo         string    `json:"attempted_repository"`
	AttemptedSurface      string    `json:"attempted_surface"`
	AttemptedFamily       string    `json:"attempted_family"`
	ContractAuthTag       string    `json:"contract_auth_tag"`
	ParentExecutionFamily string    `json:"parent_execution_family"`
	AllowedHerdrSurface   string    `json:"allowed_herdr_surface"`
	Outcome               string    `json:"outcome"`
	RecordMAC             string    `json:"record_mac"`
}

type EvidenceStore struct {
	mu     sync.Mutex
	file   *os.File
	next   int64
	key    []byte
	closed bool
}

func NewEvidenceStore(path string, key []byte) (*EvidenceStore, error) {
	if len(key) == 0 {
		return nil, errors.New("metadata key must be nonempty")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	s := &EvidenceStore{file: f, key: append([]byte(nil), key...)}
	if err := evidenceLock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if err := s.readbackLocked(); err != nil {
		unlockErr := evidenceUnlock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		return nil, errors.Join(err, unlockErr, closeErr)
	}
	if err := evidenceUnlock(int(f.Fd()), syscall.LOCK_UN); err != nil {
		closeErr := f.Close()
		return nil, errors.Join(err, closeErr)
	}
	return s, nil
}
func (s *EvidenceStore) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	err := s.file.Close()
	if err == nil {
		s.closed = true
	}
	return err
}
func (s *EvidenceStore) Append(c Contract, key []byte, attempt Attempt, reason error) (e DenialEvidence, err error) {
	if s == nil || s.file == nil {
		return DenialEvidence{}, ErrEvidence
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return DenialEvidence{}, ErrEvidence
	}
	if len(key) == 0 || !hmac.Equal(s.key, key) {
		return DenialEvidence{}, ErrInvalidContract
	}
	if err := evidenceLock(int(s.file.Fd()), syscall.LOCK_EX); err != nil {
		return DenialEvidence{}, err
	}
	defer func() {
		unlockErr := evidenceUnlock(int(s.file.Fd()), syscall.LOCK_UN)
		if unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()
	if err := s.readbackLocked(); err != nil {
		return DenialEvidence{}, err
	}
	if err := c.Verify(key); err != nil {
		return DenialEvidence{}, err
	}
	decision := c.Decide(key, attempt)
	if errors.Is(decision, ErrInvalidAttempt) {
		return DenialEvidence{}, decision
	}
	if !errors.Is(decision, ErrDenied) || !errors.Is(reason, ErrDenied) {
		return DenialEvidence{}, ErrEvidence
	}
	s.next++
	e = DenialEvidence{Repository: c.Repository, Task: c.Task, Lane: c.Lane, Role: c.Role, HerdrSession: c.HerdrSession, HerdrTab: c.HerdrTab, HerdrPane: c.HerdrPane, LeaseGeneration: c.LeaseGeneration, Sequence: s.next, Child: attempt.Child, Reason: decision.Error(), ContractDigest: c.PolicyDigest, Operation: attempt.Operation, AttemptedRepo: normalizeRepository(attempt.Repository), AttemptedSurface: normalizeEnum(attempt.Surface), AttemptedFamily: normalizeEnum(attempt.Family), ContractAuthTag: c.AuthTag, Outcome: "denied", ParentExecutionFamily: c.ParentExecutionFamily, AllowedHerdrSurface: c.AllowedHerdrSurface}
	e.RecordMAC = recordMAC(e, key)
	b, err := json.Marshal(e)
	if err != nil {
		return DenialEvidence{}, err
	}
	b = append(b, '\n')
	appendOffset, err := s.file.Seek(0, io.SeekEnd)
	if err != nil {
		s.next--
		return DenialEvidence{}, err
	}
	if n, writeErr := evidenceWrite(s.file, b); writeErr != nil || n != len(b) {
		s.next--
		rollbackErr := evidenceRollback(s.file, appendOffset)
		if rollbackErr != nil {
			primary := io.ErrShortWrite
			if writeErr != nil {
				primary = writeErr
			}
			return DenialEvidence{}, s.quarantine(errors.Join(primary, rollbackErr))
		}
		if writeErr != nil {
			return DenialEvidence{}, errors.Join(writeErr, rollbackErr)
		}
		return DenialEvidence{}, errors.Join(io.ErrShortWrite, rollbackErr)
	}
	if err = evidenceSync(s.file); err != nil {
		return DenialEvidence{}, s.quarantine(err)
	}
	if err = s.readbackLocked(); err != nil {
		return DenialEvidence{}, s.quarantine(err)
	}
	return e, nil
}

func (s *EvidenceStore) quarantine(err error) error {
	s.closed = true
	if s.file == nil {
		return err
	}
	return errors.Join(err, s.file.Close())
}
func (s *EvidenceStore) readbackLocked() error {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	data, err := io.ReadAll(s.file)
	if err != nil {
		return err
	}
	if len(data) == 0 {
		s.next = 0
		_, err := s.file.Seek(0, io.SeekEnd)
		return err
	}
	// JSONL commit framing is the trailing newline. A complete JSON object
	// without it is an uncertain crash tail, never a committed record.
	if data[len(data)-1] != '\n' {
		return ErrEvidence
	}
	lines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	var last DenialEvidence
	for _, line := range lines {
		if len(line) == 0 {
			return ErrEvidence
		}
		var e DenialEvidence
		err := json.Unmarshal(line, &e)
		if err != nil || e.Sequence != last.Sequence+1 || e.Sequence < 1 || e.Repository == "" || e.Task == "" || e.Lane == "" || e.Role == "" || e.HerdrSession == "" || e.HerdrTab == "" || e.HerdrPane == "" || e.ContractDigest == "" || e.ContractAuthTag == "" || e.ParentExecutionFamily == "" || e.AllowedHerdrSurface == "" || e.Outcome != "denied" || e.RecordMAC == "" || e.Operation == "" || e.AttemptedRepo == "" || e.AttemptedSurface == "" || e.AttemptedFamily == "" || !hmac.Equal([]byte(e.RecordMAC), []byte(recordMAC(e, s.key))) {
			return ErrEvidence
		}
		contract := Contract{Repository: e.Repository, Task: e.Task, Lane: e.Lane, Role: e.Role, LeaseGeneration: e.LeaseGeneration, HerdrSession: e.HerdrSession, HerdrTab: e.HerdrTab, HerdrPane: e.HerdrPane, ParentExecutionFamily: e.ParentExecutionFamily, AllowedHerdrSurface: e.AllowedHerdrSurface, PolicyDigest: e.ContractDigest, AuthTag: e.ContractAuthTag}
		if err := contract.Verify(s.key); err != nil {
			return ErrEvidence
		}
		decision := contract.Decide(s.key, Attempt{Operation: e.Operation, Child: e.Child, Repository: e.AttemptedRepo, Surface: e.AttemptedSurface, Family: e.AttemptedFamily})
		if errors.Is(decision, ErrInvalidAttempt) || !errors.Is(decision, ErrDenied) {
			return ErrEvidence
		}
		if err := validateAttempt(Attempt{Operation: e.Operation, Child: e.Child, Repository: e.AttemptedRepo, Surface: e.AttemptedSurface, Family: e.AttemptedFamily}); err != nil {
			return ErrEvidence
		}
		last = e
	}
	s.next = last.Sequence
	_, err = s.file.Seek(0, io.SeekEnd)
	return err
}

var evidenceWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

var evidenceSync = func(f *os.File) error { return f.Sync() }

var evidenceLock = func(fd int, how int) error { return syscall.Flock(fd, how) }

var evidenceUnlock = func(fd int, how int) error { return syscall.Flock(fd, how) }
var evidenceRollback = rollbackAppend

func rollbackAppend(f *os.File, offset int64) error {
	if err := f.Truncate(offset); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		return err
	}
	return f.Sync()
}

func authenticateDigest(digest string, key []byte) string {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(digest))
	return hex.EncodeToString(mac.Sum(nil))
}

func recordMAC(e DenialEvidence, key []byte) string {
	e.RecordMAC = ""
	b, _ := json.Marshal(e)
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write(b)
	return hex.EncodeToString(mac.Sum(nil))
}
