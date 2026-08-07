// Package signerboundary is the FAC-169 OS-enforced reviewer signing boundary.
//
// # What does NOT satisfy FAC-169 (explicit rejects)
//
//   - chmod / 0700 key dirs, env scrubbing, self-attestation
//   - same-UID signer daemons (attach/proc-mem/oracle theater)
//   - sockets callable by any same-UID process
//   - filesystem-only sandboxes: Darwin sandbox-exec (deprecated) and Linux
//     Landlock deny key-path reads but do NOT provide process attach isolation
//     or non-oracle IPC. Realpath read denial alone is NEVER full acceptance.
//
// # Acceptable mechanisms (choose one; both are OS/kernel-backed)
//
//   - separate-uid (b): three distinct kernel UIDs (signer S, requester R,
//     builder B). Private key held only by S; sign IPC authorized solely by
//     SO_PEERCRED peer UID == R (never exe path / HERD_ROLE / env). Session MAC
//     is integrity binding in R's address space only — FD handoff is not the
//     OS boundary. Live proof: B cannot induce sign-verdict; attach to S denied.
//   - keychain-acl (c, Darwin): Keychain item with a real code-signature ACL
//     and live denial for unsigned/non-allowlisted callers. Not implemented as
//     a green path until that live proof exists (BLOCKED, not fallback).
//
// Establish NEVER falls back to a weaker mechanism. Missing provisioning or
// unsupported platform → fail closed / BLOCKED.
//
// This package does not implement FAC-145 receipt TaskContext authority.
package signerboundary

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Accepted attestation mechanisms only.
const (
	MechanismSeparateUID = "separate-uid"
	MechanismKeychainACL = "keychain-acl" // Darwin option (c); BLOCKED until live ACL proof lands
	// Deprecated theater names — validateAttestation rejects these.
	mechanismBuilderSandbox = "builder-session-sandbox"
	mechanismSandboxExec    = "sandbox-exec"
	mechanismLandlock       = "landlock"
)

// Environment.
const (
	EnvSignerUID     = "HERD_SIGNER_UID"
	EnvSignerSock    = "HERD_SIGNER_SOCK"
	EnvAuthorizedExe = "HERD_SIGNER_AUTH_EXE"    // diagnostic only — not authority
	EnvSessionKey    = "HERD_SIGNER_SESSION_KEY" // forbidden bare env; use FD/stdin
	KeyDirEnv        = "HERD_KEY_DIR"
	AttestationEnv   = "HERD_ISOLATION_ATTESTATION"
)

const (
	IsolationAttestFile = "isolation.json"
	KeyFileSuffix       = ".ed25519"
	PublicKeyFile       = ".herd/receipt.pub"
	SessionKeyFile      = "session.mac" // FORBIDDEN if present — same-UID readable; use FD/stdin only
)

// Probe digest tokens required for separate-uid acceptance.
const (
	ProbeKeyUnreadable    = "key-unreadable-by-worker-uid"
	ProbeAttachDenied     = "signer-attach-denied"
	ProbeIPCAuthDenied    = "ipc-unauthorized-denied"
	ProbeAuthorizedSignOK = "ipc-authorized-sign-ok"
	ProbePathHardened     = "path-hardened-no-link"
	ProbeKeyNonExport     = "key-non-exportable"
)

var (
	ErrUnsupportedPlatform    = errors.New("signerboundary: unsupported or unprovisioned OS authority (FAC-169 BLOCKED)")
	ErrBoundaryNotEstablished = errors.New("signerboundary: boundary not established (fail-closed)")
	ErrProvisioning           = errors.New("signerboundary: provisioning failed (fail-closed)")
	ErrAdversarialSuccess     = errors.New("signerboundary: adversarial same-UID action SUCCEEDED (boundary ineffective)")
	ErrKeyExposed             = errors.New("signerboundary: key material exposed (perms/link/ownership)")
	ErrRevoked                = errors.New("signerboundary: signing authority revoked")
	ErrSelfAttestation        = errors.New("signerboundary: refusing self/chmod/fs-sandbox theater attestation")
	ErrPeerUnauthorized       = errors.New("signerboundary: peer not authorized to request signatures")
	ErrAgentRole              = errors.New("signerboundary: signing refused for agent-role process")
	ErrKeychainUnimplemented  = errors.New("signerboundary: keychain-acl not live-proved on this build (BLOCKED, no fallback)")
)

// Attestation is written only after live OS proofs succeed.
type Attestation struct {
	Mechanism      string    `json:"mechanism"`
	KeyOwnerUID    int       `json:"key_owner_uid"`
	AgentsExcluded bool      `json:"agents_excluded"`
	Platform       string    `json:"platform,omitempty"`
	SocketPath     string    `json:"socket_path,omitempty"`
	SignerPID      int       `json:"signer_pid,omitempty"`
	AuthorizedExe  string    `json:"authorized_exe,omitempty"`
	ProvedAt       time.Time `json:"proved_at"`
	ProbeDigest    string    `json:"probe_digest"`
	// ProfilePath must stay empty or repo-relative — never an absolute host path.
	ProfilePath string `json:"profile_path,omitempty"`
	// IntegrityMAC binds this attestation to the session key (not worker-forgeable).
	IntegrityMAC string `json:"integrity_mac,omitempty"`
}

// Boundary is the coordinator-side handle. In separate-uid mode the private
// key never enters this process; Sign goes through authenticated IPC.
type Boundary struct {
	mu         sync.RWMutex
	keyDir     string
	repoRoot   string
	identity   string
	pub        ed25519.PublicKey
	attest     Attestation
	socketPath string
	sessionKey SessionKey
	signerUID  int
	revoked    bool
}

// Options for Establish / Open / Provision.
type Options struct {
	KeyDir             string
	RepoRoot           string
	Identity           string
	SkipPublish        bool
	AuthorizedExe      string
	AuthorizedUIDs     []int // peers allowed to request signatures (coordinator uids)
	RequireSeparateUID bool  // default true for production Establish
	// AllowKeychain attempts Darwin keychain-acl (still BLOCKED until implemented).
	AllowKeychain bool
}

// Establish provisions or verifies OS authority and runs live adversarial
// proofs. Fails closed — never succeeds on filesystem sandbox alone.
func Establish(opts Options) (*Boundary, error) {
	if err := validateOpts(opts); err != nil {
		return nil, err
	}
	if err := refuseAgentRole(); err != nil {
		return nil, err
	}
	if err := keyDirOutsideRepo(opts.KeyDir, opts.RepoRoot); err != nil {
		return nil, err
	}

	// Default production path: separate-uid only.
	if !opts.AllowKeychain {
		opts.RequireSeparateUID = true
	}

	if opts.AllowKeychain && runtime.GOOS == "darwin" {
		// No silent fallback: keychain path must fully succeed or error.
		b, err := establishKeychainACL(opts)
		if err == nil {
			return b, nil
		}
		// If separate-uid also requested, try it; else return keychain error (BLOCKED).
		if !opts.RequireSeparateUID {
			return nil, err
		}
	}

	return establishSeparateUID(opts)
}

// Open re-verifies an existing attestation with live probes (restart path).
func Open(opts Options) (*Boundary, error) {
	att, err := readAttestationFile(opts.KeyDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBoundaryNotEstablished, err)
	}
	if err := validateAttestation(att); err != nil {
		return nil, err
	}
	return Establish(opts)
}

// SignAuthorized is the only signing entrypoint. Requires production verdict/
// receipt schema (not an arbitrary payload oracle), session MAC (never a
// same-UID-readable disk secret), and IPC peer authorization.
func (b *Boundary) SignAuthorized(req SignRequest) ([]byte, error) {
	if b == nil {
		return nil, ErrBoundaryNotEstablished
	}
	if err := refuseAgentRole(); err != nil {
		return nil, err
	}
	if err := req.ValidateProduction(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.revoked {
		return nil, ErrRevoked
	}
	if b.attest.Mechanism != MechanismSeparateUID {
		return nil, fmt.Errorf("%w: mechanism %q cannot sign", ErrBoundaryNotEstablished, b.attest.Mechanism)
	}
	return signRequestOverIPC(b.socketPath, b.sessionKey, &req)
}

// SignVerdict is the enforced reviewer consumer path (FAC-145 rebase target).
func (b *Boundary) SignVerdict(candidateSHA, baseSHA, patchID, verdict, sessionID string, payload []byte) ([]byte, error) {
	return b.SignAuthorized(NewVerdictRequest(candidateSHA, baseSHA, patchID, verdict, sessionID, payload))
}

// SignHexAuthorized is SignAuthorized with hex encoding of the signature.
func (b *Boundary) SignHexAuthorized(req SignRequest) (string, error) {
	sig, err := b.SignAuthorized(req)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(sig), nil
}

// PublicKey returns the verification key (safe to distribute to workers).
func (b *Boundary) PublicKey() ed25519.PublicKey {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make(ed25519.PublicKey, len(b.pub))
	copy(out, b.pub)
	return out
}

// Verify checks a signature with the public key.
func (b *Boundary) Verify(msg, sig []byte) error {
	pub := b.PublicKey()
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("signerboundary: no public key")
	}
	if !ed25519.Verify(pub, msg, sig) {
		return fmt.Errorf("signerboundary: signature verification failed")
	}
	return nil
}

// Attestation returns the live-proved record.
func (b *Boundary) Attestation() Attestation {
	if b == nil {
		return Attestation{}
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.attest
}

// SocketPath returns the signer IPC path.
func (b *Boundary) SocketPath() string {
	if b == nil {
		return ""
	}
	return b.socketPath
}

// KeyDir returns the key store directory.
func (b *Boundary) KeyDir() string {
	if b == nil {
		return ""
	}
	return b.keyDir
}

// PrivateKeyPath is the on-disk key path (owned by signer uid; not readable here).
func (b *Boundary) PrivateKeyPath() string {
	if b == nil {
		return ""
	}
	return PrivateKeyPath(b.keyDir, b.identity)
}

// AdversarialProbe re-runs live separate-uid proofs against the running signer.
func (b *Boundary) AdversarialProbe() error {
	if b == nil {
		return ErrBoundaryNotEstablished
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.revoked {
		return ErrRevoked
	}
	topo, err := RequireTopology()
	if err != nil {
		return err
	}
	_, _, err = proveSeparateUID(proveSepConfig{
		KeyPath:      PrivateKeyPath(b.keyDir, b.identity),
		SignerUID:    topo.SignerUID,
		RequesterUID: topo.RequesterUID,
		BuilderUID:   topo.BuilderUID,
		SocketPath:   b.socketPath,
		SessionKey:   b.sessionKey,
		SignerPID:    b.attest.SignerPID,
	})
	return err
}

func validateOpts(opts Options) error {
	if strings.TrimSpace(opts.KeyDir) == "" {
		return fmt.Errorf("signerboundary: KeyDir required")
	}
	if strings.TrimSpace(opts.RepoRoot) == "" {
		return fmt.Errorf("signerboundary: RepoRoot required")
	}
	if strings.TrimSpace(opts.Identity) == "" {
		return fmt.Errorf("signerboundary: Identity required")
	}
	return nil
}

func refuseAgentRole() error {
	if os.Getenv("HERD_ROLE") == "agent" {
		return ErrAgentRole
	}
	return nil
}

func keyDirOutsideRepo(keyDir, repoRoot string) error {
	k, r := canonPath(keyDir), canonPath(repoRoot)
	if k == r || strings.HasPrefix(k+string(filepath.Separator), r+string(filepath.Separator)) {
		return fmt.Errorf("signerboundary: key dir inside repository tree")
	}
	return nil
}

func canonPath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	dir, rest := abs, ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return abs
		}
		rest = filepath.Join(filepath.Base(dir), rest)
		dir = parent
	}
}

func validateAttestation(att Attestation) error {
	switch strings.TrimSpace(att.Mechanism) {
	case MechanismSeparateUID:
		// ok
	case MechanismKeychainACL:
		// only if fully proved — still require digest tokens
	case "", "self", "process-boundary+0700-keystore", mechanismBuilderSandbox, mechanismSandboxExec, mechanismLandlock:
		return fmt.Errorf("%w: mechanism %q (filesystem sandbox / chmod theater is not FAC-169 acceptance)", ErrSelfAttestation, att.Mechanism)
	default:
		return fmt.Errorf("%w: mechanism %q", ErrSelfAttestation, att.Mechanism)
	}
	if !att.AgentsExcluded {
		return fmt.Errorf("signerboundary: agents_excluded=false")
	}
	if strings.TrimSpace(att.ProbeDigest) == "" {
		return fmt.Errorf("signerboundary: missing live probe digest")
	}
	if strings.TrimSpace(att.IntegrityMAC) == "" {
		return fmt.Errorf("%w: attestation missing integrity MAC (worker-forgeable JSON rejected)", ErrBoundaryNotEstablished)
	}
	// Require attach + ipc auth proofs — key-read alone is insufficient.
	need := []string{ProbeKeyUnreadable, ProbeAttachDenied, ProbeIPCAuthDenied, ProbeAuthorizedSignOK, ProbePathHardened}
	if att.Mechanism == MechanismSeparateUID {
		need = append(need, ProbeKeyNonExport)
	}
	for _, n := range need {
		if !strings.Contains(att.ProbeDigest, n) {
			return fmt.Errorf("%w: probe digest missing %s (realpath read denial alone is not acceptance)", ErrBoundaryNotEstablished, n)
		}
	}
	return nil
}

func writeAttestation(keyDir string, att Attestation, sessionKey SessionKey) error {
	if att.ProfilePath != "" && filepath.IsAbs(att.ProfilePath) {
		att.ProfilePath = ""
	}
	if err := signAttestation(&att, sessionKey); err != nil {
		return err
	}
	if err := validateAttestation(att); err != nil {
		return err
	}
	path := AttestationFilePath(keyDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o770); err != nil {
		return err
	}
	return atomicWriteJSON(path, att, 0o600)
}

func readAttestationFile(keyDir string) (Attestation, error) {
	path := AttestationFilePath(keyDir)
	// Legacy flat path fallback during migration.
	data, err := os.ReadFile(path)
	if err != nil {
		legacy := filepath.Join(keyDir, IsolationAttestFile)
		data, err = os.ReadFile(legacy)
		if err != nil {
			return Attestation{}, err
		}
	}
	var att Attestation
	if err := json.Unmarshal(data, &att); err != nil {
		return Attestation{}, err
	}
	return att, nil
}

func publishPublicKey(repoRoot string, pub ed25519.PublicKey) error {
	path := filepath.Join(repoRoot, PublicKeyFile)
	want := hex.EncodeToString(pub) + "\n"
	if data, err := os.ReadFile(path); err == nil {
		if strings.TrimSpace(string(data)) == strings.TrimSpace(want) {
			return nil
		}
		return fmt.Errorf("signerboundary: published key mismatch — refuse silent rotation")
	} else if !os.IsNotExist(err) {
		return err
	}
	return atomicWriteFile(path, []byte(want), 0o644)
}

func loadPublishedPublicKey(repoRoot string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, PublicKeyFile))
	if err != nil {
		return nil, err
	}
	raw, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("signerboundary: corrupt published public key")
	}
	return ed25519.PublicKey(raw), nil
}

// newSessionKeyBytes returns 32 random bytes.
func newSessionKeyBytes() (SessionKey, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return SessionKey(b), nil
}

func shortSocketPath(identity string) (string, error) {
	base := canonPath(os.TempDir())
	dir := filepath.Join(base, "h169")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	id := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return -1
	}, strings.ToLower(identity))
	if len(id) > 4 {
		id = id[:4]
	}
	if id == "" {
		id = "k"
	}
	path := filepath.Join(dir, fmt.Sprintf("s%d%s.sock", os.Getpid()%100000, id))
	if len(path) > 100 {
		path = filepath.Join(base, fmt.Sprintf("h%d.sock", os.Getpid()%100000))
	}
	return path, nil
}

// establishKeychainACL is option (c). Until a real code-signature ACL with live
// unsigned denial is implemented, this BLOCKS (no sandbox fallback).
func establishKeychainACL(opts Options) (*Boundary, error) {
	return nil, fmt.Errorf("%w: %v", ErrKeychainUnimplemented, ErrUnsupportedPlatform)
}

// statUID is portable access to owning uid.
func statUID(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Uid), true
}

func statNlink(fi os.FileInfo) (uint64, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return uint64(st.Nlink), true
}
