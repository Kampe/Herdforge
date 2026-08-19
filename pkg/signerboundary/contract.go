package signerboundary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveKeyDir returns HERD_KEY_DIR or ~/.herd/keys.
func ResolveKeyDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv(KeyDirEnv)); dir != "" {
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("signerboundary: resolve key dir: %w", err)
	}
	return filepath.Join(home, ".herd", "keys"), nil
}

func loadSessionKeyForVerify(keyDir string) (SessionKey, error) {
	if err := refuseSessionKeyOnDisk(keyDir); err != nil {
		return nil, err
	}
	return loadCoordinatorSessionKey()
}

// RequireReady fails closed unless attestation is present, integrity-bound,
// and topology-consistent.
func RequireReady(keyDir string) (Attestation, error) {
	if strings.TrimSpace(keyDir) == "" {
		return Attestation{}, fmt.Errorf("%w: empty key dir", ErrBoundaryNotEstablished)
	}
	path := attestationPath(keyDir)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return Attestation{}, fmt.Errorf("%w: attest key not established at %s; run `herd signer-boundary establish` to establish it: %v",
				ErrBoundaryNotEstablished, path, err)
		}
		return Attestation{}, fmt.Errorf("%w: cannot read isolation attestation at %s: %v",
			ErrBoundaryNotEstablished, path, err)
	}
	var att Attestation
	if err := json.Unmarshal(data, &att); err != nil {
		return Attestation{}, fmt.Errorf("%w: corrupt attestation: %v", ErrBoundaryNotEstablished, err)
	}
	if err := validateAttestation(att); err != nil {
		return Attestation{}, err
	}
	if att.ProfilePath != "" && filepath.IsAbs(att.ProfilePath) {
		return Attestation{}, fmt.Errorf("%w: absolute ProfilePath forbidden", ErrBoundaryNotEstablished)
	}
	sk, err := loadSessionKeyForVerify(keyDir)
	if err != nil {
		return Attestation{}, fmt.Errorf("%w: session key for integrity: %v", ErrBoundaryNotEstablished, err)
	}
	if err := verifyAttestationMAC(att, sk); err != nil {
		return Attestation{}, err
	}
	// Topology must still be valid at readback.
	if _, err := RequireTopology(); err != nil {
		return Attestation{}, fmt.Errorf("%w: topology invalid at readback: %v", ErrBoundaryNotEstablished, err)
	}
	if att.Mechanism == MechanismSeparateUID && strings.TrimSpace(att.SocketPath) == "" {
		return Attestation{}, fmt.Errorf("%w: missing socket", ErrBoundaryNotEstablished)
	}
	return att, nil
}

// Ready reports whether RequireReady would succeed.
func Ready(keyDir string) bool {
	_, err := RequireReady(keyDir)
	return err == nil
}

// Provision is Establish.
func Provision(opts Options) (*Boundary, error) {
	return Establish(opts)
}

// Reprove re-runs live probes.
func Reprove(opts Options) (*Boundary, error) {
	return Open(opts)
}

// BuilderTabEnv is diagnostic only — NOT authorization. Prefer BuilderLaunchEnv
// plus kernel RunAsUID (herdr TabCreateForTask).
func BuilderTabEnv() []string {
	return BuilderLaunchEnv(nil)
}

// BuilderLaunchEnv strips signing-related env from workers. Authorization is
// HERD_BUILDER_UID process identity (setuid), not HERD_ROLE.
func BuilderLaunchEnv(base []string) []string {
	out := make([]string, 0, len(base)+2)
	for _, e := range base {
		if strings.HasPrefix(e, EnvSessionKey+"=") {
			continue
		}
		if strings.HasPrefix(e, "HERD_SIGNER_SESSION_KEY_FD=") {
			continue
		}
		if strings.HasPrefix(e, KeyDirEnv+"=") {
			continue
		}
		if strings.HasPrefix(e, EnvSignerSock+"=") {
			continue
		}
		if strings.HasPrefix(e, "HERD_ADMISSION_LEDGER=") {
			continue
		}
		out = append(out, e)
	}
	// Diagnostic only — never treat as authority.
	out = append(out, "HERD_ROLE=agent")
	return out
}

// EnforceAtLaunch is the production launch gate (fail-closed).
var EnforceAtLaunch = RequireLaunchReady

// RequireLaunchReady is fail-closed: boundary must be established and live.
// No soft-open default (FAC-169 AC: startup/readback fail closed).
func RequireLaunchReady(repoRoot string) error {
	dir, err := ResolveKeyDir()
	if err != nil {
		return fmt.Errorf("%w: %v", ErrBoundaryNotEstablished, err)
	}
	att, err := RequireReady(dir)
	if err != nil {
		return fmt.Errorf("signer boundary not ready for launch (FAC-169 fail-closed): %w", err)
	}
	// The launch gate must not accept session material Establish refuses:
	// loadSessionKeyForVerify alone honours HERD_SIGNER_INSECURE_ENV_SESSION and
	// a bare HERD_SIGNER_SESSION_KEY, both same-UID readable.
	if err := refuseInsecureSessionSources(); err != nil {
		return fmt.Errorf("%w: %v", ErrBoundaryNotEstablished, err)
	}
	sk, err := loadSessionKeyForVerify(dir)
	if err != nil {
		return fmt.Errorf("%w: session key for live re-prove: %v", ErrBoundaryNotEstablished, err)
	}
	req := SignRequest{Op: OpPing, SessionID: "launch-reprove", Payload: []byte("ping")}
	if _, err := signRequestOverIPC(att.SocketPath, sk, &req); err != nil {
		return fmt.Errorf("signer boundary live re-prove failed at launch (FAC-169 fail-closed): %w", err)
	}
	_ = repoRoot
	return nil
}

// MustNotSelfAttest rejects hand-written theater mechanisms.
func MustNotSelfAttest(mechanism string) error {
	switch strings.TrimSpace(mechanism) {
	case MechanismSeparateUID, MechanismKeychainACL:
		return fmt.Errorf("signerboundary: mechanism %q only via Establish live proof", mechanism)
	default:
		return fmt.Errorf("%w: %q", ErrSelfAttestation, mechanism)
	}
}

func attestationPath(keyDir string) string {
	return AttestationFilePath(keyDir)
}
