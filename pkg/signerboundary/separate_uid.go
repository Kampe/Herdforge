package signerboundary

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// establishSeparateUID requires kernel three-UID topology (signer/requester/builder).
func establishSeparateUID(opts Options) (*Boundary, error) {
	topo, err := RequireTopology()
	if err != nil {
		return nil, err
	}

	if err := EnsureKeyLayout(opts.KeyDir, topo); err != nil {
		return nil, err
	}
	keyPath := PrivateKeyPath(opts.KeyDir, opts.Identity)
	// Prefer private/ layout; fall back to legacy flat key path for migration.
	if _, err := os.Lstat(keyPath); err != nil {
		legacy := filepath.Join(opts.KeyDir, opts.Identity+KeyFileSuffix)
		if _, e2 := os.Lstat(legacy); e2 == nil {
			keyPath = legacy
		}
	}
	if err := auditKeyMaterialPath(keyPath, topo.SignerUID); err != nil {
		return nil, fmt.Errorf("%w: key must be owned by signer uid %d at %s: %v",
			ErrProvisioning, topo.SignerUID, keyPath, err)
	}
	if err := AuditKeyLayout(opts.KeyDir, opts.Identity, topo); err != nil {
		// AuditKeyLayout requires private/ layout; legacy flat keys skip when private missing.
		if _, e := os.Lstat(PrivateKeyPath(opts.KeyDir, opts.Identity)); e == nil {
			return nil, err
		}
	}

	// LIVE: requester uid must not read the private key (owned by signer).
	if data, err := os.ReadFile(keyPath); err == nil && len(data) > 0 {
		return nil, fmt.Errorf("%w: private key readable by requester uid %d", ErrAdversarialSuccess, os.Getuid())
	}

	pub, err := loadPublishedPublicKey(opts.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("%w: published public key required: %v", ErrProvisioning, err)
	}
	if !opts.SkipPublish {
		if err := publishPublicKey(opts.RepoRoot, pub); err != nil {
			return nil, err
		}
	}

	sock := strings.TrimSpace(os.Getenv(EnvSignerSock))
	if sock == "" {
		return nil, fmt.Errorf("%w: %s required", ErrProvisioning, EnvSignerSock)
	}

	sessionKey, err := loadOrCreateSessionKey(opts.KeyDir)
	if err != nil {
		return nil, err
	}

	signerPID := 0
	if raw := strings.TrimSpace(os.Getenv("HERD_SIGNER_PID")); raw != "" {
		if p, e := strconv.Atoi(raw); e == nil {
			signerPID = p
		}
	}

	digest, livePID, err := proveSeparateUID(proveSepConfig{
		KeyPath:      keyPath,
		SignerUID:    topo.SignerUID,
		RequesterUID: topo.RequesterUID,
		BuilderUID:   topo.BuilderUID,
		SocketPath:   sock,
		SessionKey:   sessionKey,
		SignerPID:    signerPID,
	})
	if err != nil {
		return nil, err
	}
	if livePID != 0 {
		signerPID = livePID
	}

	att := Attestation{
		Mechanism:      MechanismSeparateUID,
		KeyOwnerUID:    topo.SignerUID,
		AgentsExcluded: true,
		Platform:       goos(),
		SocketPath:     sock,
		SignerPID:      signerPID,
		// AuthorizedExe intentionally empty — exe path is not authority.
		ProvedAt:    nowUTC(),
		ProbeDigest: digest,
	}
	if err := writeAttestation(opts.KeyDir, att, sessionKey); err != nil {
		return nil, err
	}
	att, err = readAttestationFile(opts.KeyDir)
	if err != nil {
		return nil, err
	}

	return &Boundary{
		keyDir:     opts.KeyDir,
		repoRoot:   opts.RepoRoot,
		identity:   opts.Identity,
		pub:        pub,
		attest:     att,
		socketPath: sock,
		sessionKey: sessionKey,
		signerUID:  topo.SignerUID,
		// store requester for clients
	}, nil
}

func requireSignerUID() (int, error) {
	// Prefer topology; keep helper for tests that only set HERD_SIGNER_UID.
	return parseUIDEnv(EnvSignerUID, true)
}

func loadOrCreateSessionKey(keyDir string) (SessionKey, error) {
	if err := refuseSessionKeyOnDisk(keyDir); err != nil {
		return nil, err
	}
	if err := refuseInsecureSessionSources(); err != nil {
		return nil, err
	}
	return loadCoordinatorSessionKey()
}

func goos() string {
	return getenvDefault("GOOS_OVERRIDE", runtimeGOOS())
}

func runtimeGOOS() string {
	return openGOOS()
}
