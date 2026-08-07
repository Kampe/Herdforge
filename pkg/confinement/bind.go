package confinement

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// PolicyDigestV1 is the fixed policy identity for this confinement generation.
const PolicyDigestV1 = "confinement-policy-v1-fac190-r3"

// LaunchIdentity is the production binding input from launch/dispatch.
// Every field is required for write-capable workers and reviewers.
type LaunchIdentity struct {
	Repository        string
	Task              string
	LeaseGeneration   int64
	Lane              string
	Session           string
	SessionGeneration int64
	HerdrTab          string
	HerdrPane         string
	ProcessIdentity   string
	Argv              []string
	WorktreeRoot      string
	SharedRoot        string
	// AgentKind is the herdr kind (codex/grok/…) used with argv[0] for wrappers.
	AgentKind string
}

// Binding is the durable pre-launch confinement proof.
type Binding struct {
	Boundary         Boundary   `json:"-"`
	Capability       Capability `json:"-"`
	WorktreeRoot     string     `json:"worktree_root"`
	Sentinel         string     `json:"sentinel"`
	SharedRoot       string     `json:"shared_root"`
	SharedRootDigest string     `json:"shared_root_digest"`
	PolicyDigest     string     `json:"policy_digest"`
	Tuple            AuthTuple  `json:"tuple"`
	ProofNonce       string     `json:"proof_nonce"`
	// ProofMACHex is the issuer MAC over the AuthTuple (hex), so receipts are
	// not forgeable from public fields alone.
	ProofMACHex   string    `json:"proof_mac"`
	OSBackend     string    `json:"os_backend"`
	OSProved      bool      `json:"os_proved"`
	AgentWrapped  bool      `json:"agent_wrapped"`
	ProfilePath   string    `json:"profile_path,omitempty"`
	ProfileDigest string    `json:"profile_digest,omitempty"`
	WrapperBinDir string    `json:"wrapper_bin_dir,omitempty"`
	WrapperNames  []string  `json:"wrapper_names,omitempty"`
	ReceiptDigest string    `json:"receipt_digest"`
	// ReceiptMACHex authenticates the receipt with the same HMAC issuer.
	ReceiptMACHex string    `json:"receipt_mac"`
	CreatedAt     time.Time `json:"created_at"`
}

// Bind authenticates a worktree, issues a production MAC, and returns a
// capability that policy Authorize* can use. It does not install an OS sandbox
// and never writes under the shared root.
func Bind(id LaunchIdentity, issuer Issuer) (*Binding, error) {
	if issuer == nil {
		return nil, ErrUnauthenticated
	}
	if err := validateLaunchIdentity(id); err != nil {
		return nil, err
	}
	sentinelPath, err := InstallSentinel(id.WorktreeRoot)
	if err != nil {
		return nil, err
	}
	sharedDigest := ""
	if strings.TrimSpace(id.SharedRoot) != "" {
		dig, err := ObserveSharedRoot(id.SharedRoot)
		if err != nil {
			return nil, err
		}
		sharedDigest = dig
	}
	argvIdentity := argvDigest(id.Argv)
	tuple := AuthTuple{
		Repository:        id.Repository,
		Task:              id.Task,
		LeaseID:           strconv.FormatInt(id.LeaseGeneration, 10),
		Lane:              id.Lane,
		Session:           id.Session,
		SessionGeneration: strconv.FormatInt(id.SessionGeneration, 10),
		HerdrTab:          id.HerdrTab,
		HerdrPane:         id.HerdrPane,
		ProcessIdentity:   id.ProcessIdentity,
		ArgvIdentity:      argvIdentity,
		PolicyDigest:      PolicyDigestV1,
		AllowedRoots:      []string{id.WorktreeRoot},
	}
	boundary, cap, err := New(id.WorktreeRoot, sentinelPath, tuple, issuer)
	if err != nil {
		return nil, err
	}
	b := &Binding{
		Boundary:         boundary,
		Capability:       cap,
		WorktreeRoot:     cap.root,
		Sentinel:         cap.sentinel,
		SharedRoot:       strings.TrimSpace(id.SharedRoot),
		SharedRootDigest: sharedDigest,
		PolicyDigest:     PolicyDigestV1,
		Tuple:            tuple,
		ProofNonce:       cap.proof.Nonce,
		ProofMACHex:      hex.EncodeToString(cap.proof.MAC),
		CreatedAt:        time.Now().UTC(),
	}
	digest, err := b.digest()
	if err != nil {
		return nil, err
	}
	b.ReceiptDigest = digest
	return b, nil
}

func validateLaunchIdentity(id LaunchIdentity) error {
	if strings.TrimSpace(id.Repository) == "" ||
		strings.TrimSpace(id.Task) == "" ||
		id.LeaseGeneration <= 0 ||
		strings.TrimSpace(id.Lane) == "" ||
		strings.TrimSpace(id.Session) == "" ||
		id.SessionGeneration <= 0 ||
		strings.TrimSpace(id.HerdrTab) == "" ||
		strings.TrimSpace(id.HerdrPane) == "" ||
		strings.TrimSpace(id.ProcessIdentity) == "" ||
		len(id.Argv) == 0 ||
		strings.TrimSpace(id.WorktreeRoot) == "" {
		return ErrUnauthenticated
	}
	return nil
}

func argvDigest(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (b *Binding) digest() (string, error) {
	if b == nil {
		return "", fmt.Errorf("confinement: nil binding")
	}
	payload := struct {
		WorktreeRoot     string    `json:"worktree_root"`
		Sentinel         string    `json:"sentinel"`
		SharedRoot       string    `json:"shared_root"`
		SharedRootDigest string    `json:"shared_root_digest"`
		PolicyDigest     string    `json:"policy_digest"`
		Tuple            AuthTuple `json:"tuple"`
		ProofNonce       string    `json:"proof_nonce"`
		ProofMACHex      string    `json:"proof_mac"`
		OSBackend        string    `json:"os_backend"`
		OSProved         bool      `json:"os_proved"`
		AgentWrapped     bool      `json:"agent_wrapped"`
		ProfilePath      string    `json:"profile_path"`
		ProfileDigest    string    `json:"profile_digest"`
		WrapperBinDir    string    `json:"wrapper_bin_dir"`
		WrapperNames     []string  `json:"wrapper_names"`
		CreatedAt        time.Time `json:"created_at"`
	}{
		WorktreeRoot:     b.WorktreeRoot,
		Sentinel:         b.Sentinel,
		SharedRoot:       b.SharedRoot,
		SharedRootDigest: b.SharedRootDigest,
		PolicyDigest:     b.PolicyDigest,
		Tuple:            b.Tuple,
		ProofNonce:       b.ProofNonce,
		ProofMACHex:      b.ProofMACHex,
		OSBackend:        b.OSBackend,
		OSProved:         b.OSProved,
		AgentWrapped:     b.AgentWrapped,
		ProfilePath:      b.ProfilePath,
		ProfileDigest:    b.ProfileDigest,
		WrapperBinDir:    b.WrapperBinDir,
		WrapperNames:     b.WrapperNames,
		CreatedAt:        b.CreatedAt.UTC(),
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("confinement: digest marshal: %w", err)
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// SignReceipt attaches ReceiptMACHex using the HMAC issuer secret material
// already bound into the capability proof (re-derived via proof fields).
func (b *Binding) SignReceipt(issuer *HMACIssuer) error {
	if b == nil || issuer == nil {
		return ErrUnauthenticated
	}
	digest, err := b.digest()
	if err != nil {
		return err
	}
	b.ReceiptDigest = digest
	mac := hmac.New(sha256.New, issuer.secret)
	_, _ = mac.Write([]byte(digest))
	b.ReceiptMACHex = hex.EncodeToString(mac.Sum(nil))
	return nil
}

// VerifyReceiptMAC checks ReceiptMACHex against the issuer.
func (b *Binding) VerifyReceiptMAC(issuer *HMACIssuer) error {
	if b == nil || issuer == nil || b.ReceiptMACHex == "" || b.ReceiptDigest == "" {
		return ErrUnauthenticated
	}
	mac := hmac.New(sha256.New, issuer.secret)
	_, _ = mac.Write([]byte(b.ReceiptDigest))
	want := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(want), []byte(b.ReceiptMACHex)) {
		return ErrUnauthenticated
	}
	return nil
}

// AuthorizeRelativeWrite is a convenience for production callers.
func (b *Binding) AuthorizeRelativeWrite(path string) error {
	if b == nil || b.Boundary == nil {
		return ErrUnauthenticated
	}
	return b.Boundary.AuthorizeWrite(b.Capability, path)
}

// CheckSharedRoot revalidates the read-only shared-root observation.
func (b *Binding) CheckSharedRoot() error {
	if b == nil {
		return ErrUnauthenticated
	}
	if b.SharedRoot == "" {
		return nil
	}
	return CheckSharedRootObservation(b.SharedRoot, b.SharedRootDigest)
}

// PathEnv returns PATH=wrapperBin:existing for Herdr tab --env.
func (b *Binding) PathEnv(existingPATH string) string {
	if b == nil || b.WrapperBinDir == "" {
		return existingPATH
	}
	if existingPATH == "" {
		return b.WrapperBinDir
	}
	return b.WrapperBinDir + string(os.PathListSeparator) + existingPATH
}

// MarshalReceipt returns durable JSON for launch/control evidence.
func (b *Binding) MarshalReceipt() ([]byte, error) {
	if b == nil {
		return nil, fmt.Errorf("confinement: nil binding")
	}
	if b.ReceiptDigest == "" {
		digest, err := b.digest()
		if err != nil {
			return nil, err
		}
		b.ReceiptDigest = digest
	}
	return json.Marshal(b)
}
