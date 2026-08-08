package agentpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretEnv is the preferred production secret for fleet-execution contracts.
const SecretEnv = "HERD_FLEET_POLICY_SECRET"

// SecretEnvFallback lets coordinators reuse the control-plane secret when a
// dedicated fleet policy key is not configured.
const SecretEnvFallback = "HERD_CONTROL_SECRET"

// ErrMissingSecret is returned when production key material is absent.
var ErrMissingSecret = errors.New("agentpolicy: HERD_FLEET_POLICY_SECRET (or HERD_CONTROL_SECRET) is required")

// ErrMissingBinding is returned when launch/recovery has no fleet contract.
var ErrMissingBinding = errors.New("agentpolicy: fleet-execution contract is required")

// KeyFromEnv loads the HMAC key for fleet-execution contracts.
func KeyFromEnv() ([]byte, error) {
	secret := strings.TrimSpace(os.Getenv(SecretEnv))
	if secret == "" {
		secret = strings.TrimSpace(os.Getenv(SecretEnvFallback))
	}
	if secret == "" {
		return nil, ErrMissingSecret
	}
	return []byte(secret), nil
}

// LaunchBinding is the public, immutable handoff of a fleet-execution
// contract. It is safe to persist on launch receipts and recovery packets;
// it contains no key material and no absolute host paths.
type LaunchBinding struct {
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

// BindLaunch mints an authenticated contract and returns the public binding
// for receipt/recovery persistence.
func BindLaunch(repository, task, lane, role string, generation int64, session, tab, pane, family, surface string, key []byte) (LaunchBinding, Contract, error) {
	c, err := NewContract(repository, task, lane, role, generation, session, tab, pane, family, surface, key)
	if err != nil {
		return LaunchBinding{}, Contract{}, err
	}
	return bindingOf(c), c, nil
}

func bindingOf(c Contract) LaunchBinding {
	return LaunchBinding{
		Repository:            c.Repository,
		Task:                  c.Task,
		Lane:                  c.Lane,
		Role:                  c.Role,
		LeaseGeneration:       c.LeaseGeneration,
		HerdrSession:          c.HerdrSession,
		HerdrTab:              c.HerdrTab,
		HerdrPane:             c.HerdrPane,
		ParentExecutionFamily: c.ParentExecutionFamily,
		AllowedHerdrSurface:   c.AllowedHerdrSurface,
		PolicyDigest:          c.PolicyDigest,
		AuthTag:               c.AuthTag,
	}
}

// Contract reconstructs the authenticated contract from a public binding.
func (b LaunchBinding) Contract() Contract {
	return Contract{
		Repository:            b.Repository,
		Task:                  b.Task,
		Lane:                  b.Lane,
		Role:                  b.Role,
		LeaseGeneration:       b.LeaseGeneration,
		HerdrSession:          b.HerdrSession,
		HerdrTab:              b.HerdrTab,
		HerdrPane:             b.HerdrPane,
		ParentExecutionFamily: b.ParentExecutionFamily,
		AllowedHerdrSurface:   b.AllowedHerdrSurface,
		PolicyDigest:          b.PolicyDigest,
		AuthTag:               b.AuthTag,
	}
}

// Verify fails closed on absent, stale, or mismatched bindings.
func (b LaunchBinding) Verify(key []byte) error {
	if b.PolicyDigest == "" || b.AuthTag == "" {
		return ErrMissingBinding
	}
	return b.Contract().Verify(key)
}

// MatchesGeneration requires the exact lease generation from the live session.
func (b LaunchBinding) MatchesGeneration(generation int64) error {
	if err := b.VerifyFieldsPresent(); err != nil {
		return err
	}
	if b.LeaseGeneration != generation || generation < 1 {
		return fmt.Errorf("%w: lease generation mismatch (binding=%d want=%d)", ErrInvalidContract, b.LeaseGeneration, generation)
	}
	return nil
}

// VerifyFieldsPresent is a key-free structural check used when only the
// receipt shape is available (tests of serialization). Authentication still
// requires Verify(key).
func (b LaunchBinding) VerifyFieldsPresent() error {
	if b.Repository == "" || b.Task == "" || b.Lane == "" || b.Role == "" ||
		b.HerdrSession == "" || b.HerdrTab == "" || b.HerdrPane == "" ||
		b.ParentExecutionFamily == "" || b.AllowedHerdrSurface == "" ||
		b.PolicyDigest == "" || b.AuthTag == "" || b.LeaseGeneration < 1 {
		return ErrMissingBinding
	}
	return nil
}

// RequireLaunchBinding fails closed when recovery/launch cannot present an
// authenticated binding for the exact lease generation.
func RequireLaunchBinding(b LaunchBinding, key []byte, generation int64) error {
	if err := b.Verify(key); err != nil {
		return err
	}
	return b.MatchesGeneration(generation)
}

// Enforce is the single harness/tool boundary for nested-agent attempts.
// Allowed operations return nil without writing evidence. Denied nested
// agents are refused before any child is created and a durable evidence
// record is appended. Evidence append failure fails closed.
func Enforce(c Contract, key []byte, attempt Attempt, store *EvidenceStore) (DenialEvidence, error) {
	decision := c.Decide(key, attempt)
	if decision == nil {
		return DenialEvidence{}, nil
	}
	if errors.Is(decision, ErrInvalidAttempt) {
		return DenialEvidence{}, decision
	}
	if !errors.Is(decision, ErrDenied) {
		return DenialEvidence{}, decision
	}
	if store == nil {
		return DenialEvidence{}, fmt.Errorf("%w: evidence store required for nested-agent denial", ErrDenied)
	}
	return store.Append(c, key, attempt, ErrDenied)
}

// ContributorProof is the exact-SHA receipt that no hidden child contributor
// participated in a candidate. Children must be empty; a non-empty set fails
// verification and must not reach review admission.
type ContributorProof struct {
	CandidateSHA          string      `json:"candidate_sha"`
	ParentExecutionFamily string      `json:"parent_execution_family"`
	ParentSession         string      `json:"parent_session"`
	Task                  string      `json:"task"`
	LeaseGeneration       int64       `json:"lease_generation"`
	Children              []ChildKind `json:"children"`
	ProofDigest           string      `json:"proof_digest"`
}

// ProveNoHiddenContributors builds a clean proof for a candidate SHA.
// child kinds, if any, are recorded and will fail Verify.
func ProveNoHiddenContributors(candidateSHA, family, session, task string, generation int64, children []ChildKind) (ContributorProof, error) {
	sha := strings.ToLower(strings.TrimSpace(candidateSHA))
	if len(sha) != 40 {
		for _, r := range sha {
			if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
				return ContributorProof{}, errors.New("agentpolicy: candidate sha must be hex")
			}
		}
	}
	if len(sha) != 40 {
		return ContributorProof{}, errors.New("agentpolicy: candidate sha must be a full 40-character hex SHA")
	}
	for _, r := range sha {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return ContributorProof{}, errors.New("agentpolicy: candidate sha must be hex")
		}
	}
	family = normalizeEnum(family)
	session = normalizeOpaque(session)
	task = normalizeOpaque(task)
	if family == "" || session == "" || task == "" || generation < 1 {
		return ContributorProof{}, errors.New("agentpolicy: contributor proof identity is incomplete")
	}
	// Copy defensively; nil becomes empty for stable JSON.
	kids := append([]ChildKind(nil), children...)
	if kids == nil {
		kids = []ChildKind{}
	}
	p := ContributorProof{
		CandidateSHA:          sha,
		ParentExecutionFamily: family,
		ParentSession:         session,
		Task:                  task,
		LeaseGeneration:       generation,
		Children:              kids,
	}
	p.ProofDigest = p.digest()
	return p, nil
}

func (p ContributorProof) digest() string {
	body, _ := json.Marshal(struct {
		CandidateSHA, ParentExecutionFamily, ParentSession, Task string
		LeaseGeneration                                          int64
		Children                                                 []ChildKind
	}{p.CandidateSHA, p.ParentExecutionFamily, p.ParentSession, p.Task, p.LeaseGeneration, p.Children})
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Verify admits only exact-SHA proofs with zero hidden children and an
// intact digest. Any nested-agent child kind fails closed.
func (p ContributorProof) Verify() error {
	if p.ProofDigest == "" || p.ProofDigest != p.digest() {
		return errors.New("agentpolicy: contributor proof digest mismatch")
	}
	if len(p.CandidateSHA) != 40 {
		return errors.New("agentpolicy: contributor proof candidate sha is incomplete")
	}
	if len(p.Children) != 0 {
		return fmt.Errorf("agentpolicy: hidden child contributors present: %v", p.Children)
	}
	if p.ParentExecutionFamily == "" || p.ParentSession == "" || p.Task == "" || p.LeaseGeneration < 1 {
		return errors.New("agentpolicy: contributor proof identity is incomplete")
	}
	return nil
}
