package verifier

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	HermeticReceiptVersion = 1
	// HermeticReceiptMaxTTL is compiled policy, not receipt/request input.
	HermeticReceiptMaxTTL = 15 * time.Minute
)

var ErrHostExecutionDenied = errors.New("native fixture execution is denied by the hermetic host boundary")

type IsolationKind string

const (
	IsolationContainer IsolationKind = "container"
	IsolationVM        IsolationKind = "vm"
)

// IsolationBinding uses a discriminator plus two slots so both/neither
// identity mistakes are representable and rejected before signature use.
type IsolationBinding struct {
	Kind              IsolationKind `json:"kind"`
	ContainerIdentity string        `json:"container_identity,omitempty"`
	VMIdentity        string        `json:"vm_identity,omitempty"`
}

func (i IsolationBinding) validate() error {
	switch i.Kind {
	case IsolationContainer:
		if i.ContainerIdentity == "" || i.VMIdentity != "" {
			return errors.New("container isolation requires exactly one container identity")
		}
	case IsolationVM:
		if i.VMIdentity == "" || i.ContainerIdentity != "" {
			return errors.New("VM isolation requires exactly one VM identity")
		}
	default:
		return fmt.Errorf("unsupported isolation kind %q", i.Kind)
	}
	return nil
}

// HermeticReceiptV1 is signed evidence from a trusted launcher. PayloadDigest
// is only a checksum; Signature, verified against external compiled authority,
// is the authentication boundary.
type HermeticReceiptV1 struct {
	Version                  int              `json:"version"`
	Repository               string           `json:"repository"`
	Task                     string           `json:"task"`
	CandidateSHA             string           `json:"candidate_sha"`
	Argv                     []string         `json:"argv"`
	ArgvDigest               string           `json:"argv_digest"`
	Isolation                IsolationBinding `json:"isolation"`
	PIDNamespaceIdentity     string           `json:"pid_namespace_identity"`
	UserNamespaceIdentity    string           `json:"user_namespace_identity"`
	UID                      int              `json:"uid"`
	GID                      int              `json:"gid"`
	NetworkMode              string           `json:"network_mode"`
	MountPolicy              string           `json:"mount_policy"`
	SourceCopyDigest         string           `json:"source_copy_digest"`
	StartedAt                time.Time        `json:"started_at"`
	ExpiresAt                time.Time        `json:"expires_at"`
	Nonce                    string           `json:"nonce"`
	Generation               string           `json:"generation"`
	HostPIDSharing           bool             `json:"host_pid_sharing"`
	HostUserNamespaceSharing bool             `json:"host_user_namespace_sharing"`
	PayloadDigest            string           `json:"payload_digest"`
	Signature                []byte           `json:"signature"`
}

// HermeticAdmissionRequest is complete by construction: every security field
// is required and compared exactly. It deliberately has no key field; key
// selection belongs to compiled launcher authority outside this request.
type HermeticAdmissionRequest struct {
	Repository               string
	Task                     string
	CandidateSHA             string
	Argv                     []string
	ArgvDigest               string
	Isolation                IsolationBinding
	PIDNamespaceIdentity     string
	UserNamespaceIdentity    string
	UID                      int
	GID                      int
	NetworkMode              string
	MountPolicy              string
	SourceCopyDigest         string
	Generation               string
	Nonce                    string
	HostPIDSharing           bool
	HostUserNamespaceSharing bool
}

// TrustedReceiptVerifier owns the fixed launcher key outside both receipt and
// request. Callers cannot select a key through admission inputs.
type TrustedReceiptVerifier struct {
	keyID     string
	publicKey ed25519.PublicKey
}

func NewTrustedReceiptVerifier(publicKey ed25519.PublicKey) (*TrustedReceiptVerifier, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("compiled launcher authority key is missing or invalid")
	}
	keyCopy := append(ed25519.PublicKey(nil), publicKey...)
	sum := sha256.Sum256(keyCopy)
	return &TrustedReceiptVerifier{keyID: "sha256:" + hex.EncodeToString(sum[:]), publicKey: keyCopy}, nil
}

func (v *TrustedReceiptVerifier) validate() error {
	if v == nil || v.keyID == "" || len(v.publicKey) != ed25519.PublicKeySize {
		return errors.New("compiled launcher authority key is missing or invalid")
	}
	return nil
}

func (v *TrustedReceiptVerifier) authorityKeyID() string {
	if v == nil {
		return ""
	}
	return v.keyID
}

type ReplayToken struct {
	Generation string
	Nonce      string
	Payload    string
}

type ReplayResult uint8

const (
	ReplayFresh ReplayResult = iota + 1
	ReplayDuplicate
	ReplayConflict
	ReplayPersistenceFailure
)

// ReplayAuthority must provide atomic durable one-shot consumption. Production
// implementations must not be process-local; test fakes may implement it in
// memory to exercise the admission protocol.
type ReplayAuthority interface {
	ConsumeOnce(context.Context, ReplayToken) (ReplayResult, error)
}

func (r HermeticReceiptV1) Validate(now time.Time) error {
	if r.Version != HermeticReceiptVersion {
		return fmt.Errorf("unsupported hermetic receipt version %d", r.Version)
	}
	req := HermeticAdmissionRequest{
		Repository: r.Repository, Task: r.Task, CandidateSHA: r.CandidateSHA,
		Argv: r.Argv, ArgvDigest: r.ArgvDigest, Isolation: r.Isolation,
		PIDNamespaceIdentity: r.PIDNamespaceIdentity, UserNamespaceIdentity: r.UserNamespaceIdentity,
		UID: r.UID, GID: r.GID, NetworkMode: r.NetworkMode, MountPolicy: r.MountPolicy,
		SourceCopyDigest: r.SourceCopyDigest, Generation: r.Generation, Nonce: r.Nonce,
		HostPIDSharing: r.HostPIDSharing, HostUserNamespaceSharing: r.HostUserNamespaceSharing,
	}
	if err := req.validate(); err != nil {
		return err
	}
	if r.StartedAt.IsZero() || r.ExpiresAt.IsZero() || now.IsZero() {
		return errors.New("hermetic receipt validity timestamps are incomplete")
	}
	if now.Before(r.StartedAt) || !now.Before(r.ExpiresAt) {
		return errors.New("hermetic receipt is outside its validity window")
	}
	if r.ExpiresAt.Sub(r.StartedAt) > HermeticReceiptMaxTTL {
		return errors.New("hermetic receipt validity window exceeds compiled maximum TTL")
	}
	if r.HostPIDSharing || r.HostUserNamespaceSharing {
		return errors.New("hermetic receipt shares a host namespace")
	}
	if len(r.Signature) != ed25519.SignatureSize {
		return errors.New("hermetic receipt signature authority is incomplete")
	}
	if !validDigest(r.PayloadDigest) || r.PayloadDigest != payloadDigest(r) {
		return errors.New("hermetic receipt payload checksum is invalid")
	}
	return nil
}

func (r HermeticReceiptV1) ValidateFor(req HermeticAdmissionRequest, now time.Time, verifier *TrustedReceiptVerifier) error {
	if err := req.validate(); err != nil {
		return err
	}
	if err := r.Validate(now); err != nil {
		return err
	}
	if err := verifier.validate(); err != nil {
		return err
	}
	if !sameRequestValues(r, req) {
		return errors.New("hermetic receipt does not bind the complete admission request")
	}
	if !ed25519.Verify(verifier.publicKey, signedPayload(r), r.Signature) {
		return errors.New("hermetic receipt signature is invalid")
	}
	return nil
}

// AdmitBeforeFixtureConstruction is the only admission callback seam. It
// authenticates and validates all policy, atomically consumes replay authority,
// and only then permits construction. No runner contract or caller booleans
// can authorize this function.
func AdmitBeforeFixtureConstruction(ctx context.Context, receipt HermeticReceiptV1, req HermeticAdmissionRequest, verifier *TrustedReceiptVerifier, replay ReplayAuthority, now time.Time, construct func() error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := receipt.ValidateFor(req, now, verifier); err != nil {
		return err
	}
	if replay == nil {
		return errors.New("durable replay authority is required")
	}
	if construct == nil {
		return errors.New("fixture construction callback is required")
	}
	result, err := replay.ConsumeOnce(ctx, ReplayToken{Generation: receipt.Generation, Nonce: receipt.Nonce, Payload: receipt.PayloadDigest})
	if err != nil || result == ReplayPersistenceFailure {
		if err != nil {
			return fmt.Errorf("replay authority persistence failure: %w", err)
		}
		return errors.New("replay authority persistence failure")
	}
	switch result {
	case ReplayFresh:
		return construct()
	case ReplayDuplicate:
		return errors.New("hermetic receipt replay detected")
	case ReplayConflict:
		return errors.New("hermetic receipt replay identity conflicts with prior payload")
	default:
		return errors.New("replay authority returned an invalid result")
	}
}

func (r HermeticReceiptV1) ValidateDigest() error {
	if !validDigest(r.PayloadDigest) || r.PayloadDigest != payloadDigest(r) {
		return errors.New("hermetic receipt payload checksum is invalid")
	}
	return nil
}

func (r HermeticReceiptV1) signedPayload() []byte {
	copy := r
	copy.Signature = nil
	b, _ := json.Marshal(copy)
	return b
}

func signedPayload(r HermeticReceiptV1) []byte { return r.signedPayload() }

func payloadDigest(r HermeticReceiptV1) string {
	r.PayloadDigest = ""
	r.Signature = nil
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func digestArgv(argv []string) string {
	b, _ := json.Marshal(argv)
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (r HermeticAdmissionRequest) validate() error {
	if r.Repository == "" || r.Task == "" || !validSHA(r.CandidateSHA) {
		return errors.New("hermetic admission request identity is incomplete")
	}
	if len(r.Argv) == 0 || r.Argv[0] == "" || r.ArgvDigest != digestArgv(r.Argv) || !validDigest(r.ArgvDigest) {
		return errors.New("hermetic admission request argv binding is invalid")
	}
	if err := r.Isolation.validate(); err != nil {
		return err
	}
	if r.PIDNamespaceIdentity == "" || r.UserNamespaceIdentity == "" || r.UID <= 0 || r.GID <= 0 {
		return errors.New("hermetic admission request namespace or UID/GID binding is incomplete")
	}
	if r.NetworkMode != "none" || r.MountPolicy != "immutable-copy-no-host-bind" {
		return errors.New("hermetic admission request host policy is not deny-by-default")
	}
	if !validDigest(r.SourceCopyDigest) || r.Generation == "" || r.Nonce == "" {
		return errors.New("hermetic admission request source or replay binding is incomplete")
	}
	if r.HostPIDSharing || r.HostUserNamespaceSharing {
		return errors.New("hermetic admission request shares a host namespace")
	}
	return nil
}

func sameRequestValues(r HermeticReceiptV1, req HermeticAdmissionRequest) bool {
	return r.Repository == req.Repository && r.Task == req.Task && r.CandidateSHA == req.CandidateSHA &&
		sameStrings(r.Argv, req.Argv) && r.ArgvDigest == req.ArgvDigest &&
		r.Isolation == req.Isolation && r.PIDNamespaceIdentity == req.PIDNamespaceIdentity &&
		r.UserNamespaceIdentity == req.UserNamespaceIdentity && r.UID == req.UID && r.GID == req.GID &&
		r.NetworkMode == req.NetworkMode && r.MountPolicy == req.MountPolicy &&
		r.SourceCopyDigest == req.SourceCopyDigest && r.Generation == req.Generation && r.Nonce == req.Nonce &&
		r.HostPIDSharing == req.HostPIDSharing && r.HostUserNamespaceSharing == req.HostUserNamespaceSharing
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}
