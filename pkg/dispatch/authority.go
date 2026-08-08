package dispatch

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

var randRead = rand.Read

// FAC-145 receipt authority. Signing is asymmetric (ed25519): the PRIVATE
// key lives OUTSIDE the repository tree — every agent worktree sits under
// the repo, so no path a worker is confined to contains signing material.
// Only the PUBLIC key is published into the repo for verification. A worker
// that reads every file it can reach obtains verify-only material and
// cannot issue or alter a receipt.
//
// KeyDirEnv overrides the private key directory (tests, multi-coordinator
// setups). Default: ~/.herd/keys.
const KeyDirEnv = "HERD_KEY_DIR"

// ReceiptPubFile is the published verification key, relative to repo root.
const ReceiptPubFile = ".herd/receipt.pub"

// resolveKeyDir returns the private-key directory for this user.
func resolveKeyDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(KeyDirEnv)); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve key dir: %w", err)
	}
	return filepath.Join(home, ".herd", "keys"), nil
}

// Signer holds the coordinator's private receipt key.
type Signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

// LoadOrCreateSigner loads (or creates exactly once) the private key for
// repoName in keyDir, and publishes/validates the public key at
// repoRoot/.herd/receipt.pub. Key creation is exclusive (O_EXCL) so a
// concurrent racer re-reads instead of rotating; ANY unexpected read error
// is fatal — authority is never silently regenerated. An existing published
// public key that does not match the private key is a hard error (refuse to
// rotate authority), never an overwrite.
func LoadOrCreateSigner(keyDir, repoName, repoRoot string) (*Signer, error) {
	if strings.TrimSpace(repoName) == "" {
		return nil, fmt.Errorf("signer requires a repository name")
	}
	if err := signerBoundaryCheck(keyDir, repoRoot); err != nil {
		return nil, err
	}
	// Signing authority must be demonstrably isolated — checked here, not
	// deferred to FAC-133 landing.
	if err := requireIsolatedKeyStore(keyDir); err != nil {
		return nil, err
	}
	_ = os.MkdirAll(keyDir, 0700)
	if err := requireIsolatedKeyStore(keyDir); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(keyDir, repoName+".ed25519")

	seed, err := readKeySeed(keyPath)
	if os.IsNotExist(err) {
		if mkErr := os.MkdirAll(keyDir, 0700); mkErr != nil {
			return nil, fmt.Errorf("create key dir: %w", mkErr)
		}

		fresh := make([]byte, ed25519.SeedSize)
		if _, rErr := randRead(fresh); rErr != nil {
			return nil, fmt.Errorf("generate key: %w", rErr)
		}
		f, oErr := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if oErr != nil {
			if os.IsExist(oErr) {
				// Lost the creation race: use the winner's key, never rotate.
				seed, err = readKeySeed(keyPath)
				if err != nil {
					return nil, fmt.Errorf("read raced key: %w", err)
				}
				return finishSigner(seed, repoRoot)
			}
			return nil, fmt.Errorf("create key: %w", oErr)
		}
		if _, wErr := f.WriteString(hex.EncodeToString(fresh) + "\n"); wErr != nil {
			f.Close()
			return nil, fmt.Errorf("write key: %w", wErr)
		}
		if sErr := f.Sync(); sErr != nil {
			f.Close()
			return nil, fmt.Errorf("sync key: %w", sErr)
		}
		if cErr := f.Close(); cErr != nil {
			return nil, fmt.Errorf("close key: %w", cErr)
		}
		seed = fresh
	} else if err != nil {
		return nil, fmt.Errorf("read key %s: %w", keyPath, err)
	}
	return finishSigner(seed, repoRoot)
}

// RepositoryIdentity is a STABLE identity for the repository at root: the
// normalized origin remote plus the configured name, hashed. Two different
// repositories that happen to share a configured name get different
// identities — and therefore different signing keys and different binding
// authority — closing the cross-repository redirect hole (FAC-145).
// Falls back to the root's own inode-stable path when no remote exists.
func RepositoryIdentity(repoRoot, configuredName string) (string, error) {
	name := strings.TrimSpace(configuredName)
	if name == "" {
		return "", fmt.Errorf("repository identity requires a configured project name (FAC-145)")
	}
	origin := ""
	if out, err := exec.Command("git", "-C", repoRoot, "remote", "get-url", "origin").Output(); err == nil {
		origin = normalizeRemote(strings.TrimSpace(string(out)))
	}
	if origin == "" {
		// No remote: fall back to the repository's COMMON git dir, which is
		// shared by every worktree of the same repository — a per-worktree
		// path would mint a different identity (and key) per worktree.
		out, err := exec.Command("git", "-C", repoRoot, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
		if err != nil {
			return "", fmt.Errorf("repository identity: no origin and no git common dir at %s: %w", repoRoot, err)
		}
		common := strings.TrimSpace(string(out))
		if resolved, rErr := filepath.EvalSymlinks(common); rErr == nil {
			common = resolved
		}
		origin = "local:" + common
	}
	// 128-bit namespace: an R3 authority identity must not be trivially
	// collidable by a chosen-name attacker.
	sum := sha256.Sum256([]byte(strings.ToLower(name) + "\x00" + origin))
	return fmt.Sprintf("%s-%s", sanitizeIdentity(name), hex.EncodeToString(sum[:16])), nil
}

// normalizeRemote strips transport, credentials, and .git so ssh and https
// forms of the same repository produce one identity.
func normalizeRemote(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, ".git")
	u = strings.TrimPrefix(u, "ssh://")
	u = strings.TrimPrefix(u, "https://")
	u = strings.TrimPrefix(u, "http://")
	u = strings.TrimPrefix(u, "git://")
	if at := strings.Index(u, "@"); at >= 0 && !strings.Contains(u[:at], "/") {
		u = u[at+1:]
	}
	u = strings.ReplaceAll(u, ":", "/")
	return strings.ToLower(strings.Trim(u, "/"))
}

func sanitizeIdentity(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

// LoadSignerForConfig is the production entry: key dir from env/home, and
// the STABLE repository identity (not just the configured name) as the key
// selector, so same-named foreign repositories never share signing material.
func LoadSignerForConfig(repoName, repoRoot string) (*Signer, error) {
	dir, err := resolveKeyDir()
	if err != nil {
		return nil, err
	}
	identity, err := RepositoryIdentity(repoRoot, repoName)
	if err != nil {
		return nil, err
	}
	return LoadOrCreateSigner(dir, identity, repoRoot)
}

// IsolationAttestFile records the OS-level isolation an operator has
// actually put in place for signing material. FAC-133 writes it from its
// sandbox; until then an operator may attest manually. Signing REFUSES
// when the key directory is not demonstrably isolated (FAC-145): the
// authority does not depend on FAC-133 landing, only on the isolation
// being real and checkable.
const IsolationAttestFile = "isolation.json"

// AttestationEnv overrides where the external isolation attestation lives.
const AttestationEnv = "HERD_ISOLATION_ATTESTATION"

// isolationAttestation is the on-disk attestation next to the key store.
type isolationAttestation struct {
	// Mechanism names the enforced boundary (e.g. "sandbox-exec",
	// "separate-uid", "keychain"). Free-form but recorded.
	Mechanism string `json:"mechanism"`
	// KeyOwnerUID is the uid that owns the key store.
	KeyOwnerUID int `json:"key_owner_uid"`
	// AgentsExcluded asserts agent processes cannot read the key store.
	AgentsExcluded bool `json:"agents_excluded"`
}

// AttestationEnv points at an attestation issued OUTSIDE this process
// (FAC-133 sandbox profile, an OS keychain policy, or an operator-managed
// file). Herdforge NEVER writes an "agents excluded" claim about itself:
// 0700 excludes other uids, not same-uid agents, so self-attestation would
// be a lie. Signing therefore fails closed until a real boundary exists.

// requireIsolatedKeyStore fails closed unless the key directory is private
// to this uid AND an EXTERNAL attestation states that same-uid agent
// processes cannot read it. Absence of that attestation is a refusal, not
// a default-allow.
func requireIsolatedKeyStore(keyDir string) error {
	fi, err := os.Lstat(keyDir)
	if os.IsNotExist(err) {
		return nil // first use: creation below applies 0700 + attestation
	}
	if err != nil {
		return fmt.Errorf("cannot audit key store %s (FAC-145 fail-closed): %w", keyDir, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("key store %s must be a real directory (FAC-145)", keyDir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("key store %s is owned by uid %d, not this coordinator uid %d — refusing to sign with foreign-owned authority (FAC-145)", keyDir, st.Uid, os.Getuid())
	}
	if fi.Mode().Perm()&0o077 != 0 {
		// We own it: tighten rather than run with exposed signing material.
		if cErr := os.Chmod(keyDir, 0700); cErr != nil {
			return fmt.Errorf("key store %s is group/world accessible (%v) and could not be tightened — signing material is not isolated (FAC-145): %w", keyDir, fi.Mode().Perm(), cErr)
		}
	}
	attestPath := filepath.Join(keyDir, IsolationAttestFile)
	if env := strings.TrimSpace(os.Getenv(AttestationEnv)); env != "" {
		attestPath = env
	}
	data, err := os.ReadFile(attestPath)
	if os.IsNotExist(err) {
		return fmt.Errorf("key store %s has no isolation attestation at %s — same-uid agents can read it, so signing is REFUSED until an external boundary (FAC-133 sandbox, OS keychain, or operator attestation) is in force (FAC-145 fail-closed)", keyDir, attestPath)
	}
	if err != nil {
		return fmt.Errorf("cannot read key store attestation (FAC-145 fail-closed): %w", err)
	}
	var att isolationAttestation
	if jErr := json.Unmarshal(data, &att); jErr != nil {
		return fmt.Errorf("key store attestation is corrupt (FAC-145 fail-closed): %w", jErr)
	}
	switch att.Mechanism {
	case "", "process-boundary+0700-keystore", "self":
		return fmt.Errorf("key store %s carries a SELF-asserted attestation (%q) — 0700 excludes other uids, not same-uid agents; an external boundary is required (FAC-145 fail-closed)", keyDir, att.Mechanism)
	}
	if !att.AgentsExcluded {
		return fmt.Errorf("key store %s attests agents are NOT excluded (mechanism %q) — refusing to sign with non-isolated authority (FAC-145)", keyDir, att.Mechanism)
	}
	if att.KeyOwnerUID != os.Getuid() {
		return fmt.Errorf("key store %s is attested for uid %d, not this coordinator uid %d (FAC-145)", keyDir, att.KeyOwnerUID, os.Getuid())
	}
	return nil
}

// WriteIsolationAttestation records an EXTERNALLY established boundary.
// It is called by the party that actually enforces the boundary (FAC-133's
// sandbox, an operator provisioning step, or a test that simulates one) —
// never by the signer about itself.
func WriteIsolationAttestation(keyDir, mechanism string) error {
	switch strings.TrimSpace(mechanism) {
	case "", "process-boundary+0700-keystore", "self":
		return fmt.Errorf("refusing to record a self-asserted isolation mechanism %q (FAC-145)", mechanism)
	}
	if err := os.MkdirAll(keyDir, 0700); err != nil {
		return err
	}
	att := isolationAttestation{
		Mechanism:      mechanism,
		KeyOwnerUID:    os.Getuid(),
		AgentsExcluded: true,
	}
	data, err := json.MarshalIndent(att, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(keyDir, IsolationAttestFile), append(data, '\n'), 0600)
}

// signerBoundaryCheck enforces the coordinator-only signing boundary that
// is achievable under a single Unix user:
//  1. the private key dir must live OUTSIDE the repo tree every agent can
//     reach (worktrees are under the repo);
//  2. signing is REFUSED from a process whose working directory is inside
//     the repo's managed agent worktrees — a spawned agent invoking the
//     herd CLI cannot mint or widen receipts.
//
// True same-UID isolation requires OS-level separation (distinct users or
// sandbox profiles) and is a host deployment concern; this boundary makes
// every in-fleet path fail closed and is proven by tests.
func signerBoundaryCheck(keyDir, repoRoot string) error {
	// canon resolves symlinks via the deepest EXISTING ancestor so paths
	// that do not exist yet still canonicalize (macOS /var -> /private/var).
	canon := func(p string) string {
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
	keyAbs, rootAbs := canon(keyDir), canon(repoRoot)
	if keyAbs == rootAbs || strings.HasPrefix(keyAbs+string(filepath.Separator), rootAbs+string(filepath.Separator)) {
		return fmt.Errorf("receipt key dir %s is inside the repository tree — signing material must live outside every agent-reachable path (FAC-145)", keyDir)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("resolve cwd: %w", err)
	}
	cwd = canon(cwd)
	agentRoot := filepath.Join(rootAbs, ".herd", "worktrees") + string(filepath.Separator)
	if strings.HasPrefix(cwd+string(filepath.Separator), agentRoot) {
		return fmt.Errorf("signing refused from managed agent worktree %s (FAC-145 coordinator-only boundary)", cwd)
	}
	// Herdr stamps HERD_ROLE=agent into every task pane at creation; the
	// inherited environment refuses signing for spawned agents regardless
	// of cwd. This is a role MARKER, not identity — the non-bypassable
	// boundary is FAC-133's sandbox; do not treat this check as
	// coordinator-only authority on its own.
	if os.Getenv("HERD_ROLE") == "agent" {
		return fmt.Errorf("signing refused for an agent-role process (FAC-145 coordinator-only boundary)")
	}
	return nil
}

func readKeySeed(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Permission audit fails CLOSED: an unstat-able key is as untrusted as
	// an exposed one.
	fi, sErr := os.Stat(path)
	if sErr != nil {
		return nil, fmt.Errorf("cannot audit receipt key permissions at %s (FAC-145 fail-closed): %w", path, sErr)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("receipt key %s is group/world accessible (%v) — refusing to use exposed signing material (FAC-145)", path, fi.Mode().Perm())
	}
	seed, decErr := hex.DecodeString(strings.TrimSpace(string(data)))
	if decErr != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("key material at %s is corrupt — refusing to guess or rotate", path)
	}
	return seed, nil
}

func finishSigner(seed []byte, repoRoot string) (*Signer, error) {
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	if err := publishPublicKey(repoRoot, pub); err != nil {
		return nil, err
	}
	return &Signer{priv: priv, pub: pub}, nil
}

// publishPublicKey writes the verification key into the repo, refusing to
// replace a DIFFERENT existing key (that would be silent authority rotation).
func publishPublicKey(repoRoot string, pub ed25519.PublicKey) error {
	path := filepath.Join(repoRoot, ReceiptPubFile)
	want := hex.EncodeToString(pub)
	if data, err := os.ReadFile(path); err == nil {
		have := strings.TrimSpace(string(data))
		if have == want {
			return nil
		}
		return fmt.Errorf("published receipt key %s does not match the private key — refusing to rotate authority; resolve key state manually", path)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("read published key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create pub dir: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".receipt.pub.tmp-*")
	if err != nil {
		return fmt.Errorf("stage pub key: %w", err)
	}
	if _, err := tmp.WriteString(want + "\n"); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("write pub key: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return fmt.Errorf("sync pub key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("close pub key: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return fmt.Errorf("publish pub key: %w", err)
	}
	// Directory fsync so the publication survives a crash — a failed sync
	// fails the publication (the verification anchor must never be
	// observable as missing after issuance).
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open pub dir for sync: %w", err)
	}
	if err := dir.Sync(); err != nil {
		dir.Close()
		return fmt.Errorf("sync pub dir: %w", err)
	}
	return dir.Close()
}

// Issue validates and signs a receipt with the coordinator's private key.
func (s *Signer) Issue(tc TaskContext) (TaskContext, error) {
	if s == nil || len(s.priv) == 0 {
		return tc, fmt.Errorf("no signer — receipts cannot be issued (FAC-145)")
	}
	tc.Signature = ""
	if err := tc.Validate(); err != nil {
		return tc, err
	}
	canonical, err := canonicalReceipt(tc)
	if err != nil {
		return tc, err
	}
	tc.Signature = hex.EncodeToString(ed25519.Sign(s.priv, canonical))
	return tc, nil
}

// IssueCoordinator verifies an agent receipt against this signer's own
// public key and re-issues it widened to coordinator authority. This is the
// ONLY way a receipt gains ops: authenticated issuance by the key holder —
// there is no field-rewrite path.
func (s *Signer) IssueCoordinator(from TaskContext) (TaskContext, error) {
	if s == nil {
		return from, fmt.Errorf("no signer (FAC-145)")
	}
	if err := (&Verifier{pub: s.pub}).Verify(from); err != nil {
		return from, err
	}
	from.Role = RoleCoordinator
	from.AllowedOps = append([]string(nil), CoordinatorOps...)
	return s.Issue(from)
}

// SignBytes signs arbitrary canonical bytes (journal records, lifecycle
// state) with the coordinator key.
func (s *Signer) SignBytes(data []byte) (string, error) {
	if s == nil || len(s.priv) == 0 {
		return "", fmt.Errorf("no signer (FAC-145)")
	}
	return hex.EncodeToString(ed25519.Sign(s.priv, data)), nil
}

// VerifyBytes authenticates bytes signed with SignBytes.
func (v *Verifier) VerifyBytes(data []byte, sigHex string) error {
	if v == nil || len(v.pub) != ed25519.PublicKeySize {
		return fmt.Errorf("no verification key (FAC-145)")
	}
	sig, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil {
		return fmt.Errorf("malformed signature")
	}
	if !ed25519.Verify(v.pub, data, sig) {
		return fmt.Errorf("signature verification failed — tampered or foreign record (FAC-145)")
	}
	return nil
}

// Verifier authenticates receipts with the published public key.
type Verifier struct {
	pub ed25519.PublicKey
}

// NewVerifier wraps raw public key bytes (tests, embedded anchors).
func NewVerifier(pub ed25519.PublicKey) *Verifier { return &Verifier{pub: pub} }

// LoadVerifier reads the repo's published verification key. Missing key =
// no authority anchor = error; consumers fail closed.
func LoadVerifier(repoRoot string) (*Verifier, error) {
	data, err := os.ReadFile(filepath.Join(repoRoot, ReceiptPubFile))
	if err != nil {
		return nil, fmt.Errorf("no receipt verification key at %s (FAC-145: cannot authenticate receipts): %w", filepath.Join(repoRoot, ReceiptPubFile), err)
	}
	raw, decErr := hex.DecodeString(strings.TrimSpace(string(data)))
	if decErr != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("receipt verification key is corrupt")
	}
	return &Verifier{pub: ed25519.PublicKey(raw)}, nil
}

// Verify fails closed on an unsigned, forged, or tampered receipt.
func (v *Verifier) Verify(tc TaskContext) error {
	if v == nil || len(v.pub) != ed25519.PublicKeySize {
		return fmt.Errorf("no verification key — refusing to trust receipt for %s (FAC-145)", tc.TaskRef)
	}
	sigHex := strings.TrimSpace(tc.Signature)
	if sigHex == "" {
		return fmt.Errorf("receipt for %s is unsigned (FAC-145: only coordinator-issued receipts carry authority)", tc.TaskRef)
	}
	sig, err := hex.DecodeString(sigHex)
	if err != nil {
		return fmt.Errorf("receipt for %s carries a malformed signature", tc.TaskRef)
	}
	tc.Signature = ""
	canonical, err := canonicalReceipt(tc)
	if err != nil {
		return err
	}
	if !ed25519.Verify(v.pub, canonical, sig) {
		return fmt.Errorf("receipt for %s failed signature verification — tampered or foreign receipt rejected (FAC-145)", tc.TaskRef)
	}
	return nil
}

func canonicalReceipt(tc TaskContext) ([]byte, error) {
	tc.Signature = ""
	data, err := json.Marshal(tc)
	if err != nil {
		return nil, fmt.Errorf("canonicalize receipt: %w", err)
	}
	return data, nil
}
