package signerboundary

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// Key store layout (FAC-169 finding #6):
//
//	$keyDir/
//	  private/                 dir mode 0700, owner S
//	    <identity>.ed25519     mode 0600, owner S  (seed never R-readable)
//	  attest/                  dir mode 0770, owner R:SocketGID (or R-owned 0700)
//	    isolation.json         mode 0600, writer R
//
// R may write attestation without holding the private seed. S alone opens seed.

const (
	PrivateSubdir = "private"
	AttestSubdir  = "attest"
)

// PrivateKeyPath returns $keyDir/private/<identity>.ed25519
func PrivateKeyPath(keyDir, identity string) string {
	return filepath.Join(keyDir, PrivateSubdir, identity+KeyFileSuffix)
}

// AttestationFilePath returns $keyDir/attest/isolation.json
func AttestationFilePath(keyDir string) string {
	if env := strings.TrimSpace(os.Getenv(AttestationEnv)); env != "" {
		return env
	}
	return filepath.Join(keyDir, AttestSubdir, IsolationAttestFile)
}

// EnsureKeyLayout creates private/ and attest/ with intended modes when running
// as an operator that can chown (typically root launcher). Non-root callers get
// best-effort mkdir only.
func EnsureKeyLayout(keyDir string, topo Topology) error {
	if err := os.MkdirAll(keyDir, 0o755); err != nil {
		return err
	}
	priv := filepath.Join(keyDir, PrivateSubdir)
	attest := filepath.Join(keyDir, AttestSubdir)
	if err := os.MkdirAll(priv, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(attest, 0o770); err != nil {
		return err
	}
	// Best-effort ownership when privileged.
	_ = os.Chown(priv, topo.SignerUID, topo.SocketGID)
	_ = os.Chmod(priv, 0o700)
	_ = os.Chown(attest, topo.RequesterUID, topo.SocketGID)
	_ = os.Chmod(attest, 0o770)
	return nil
}

// AuditKeyLayout proves private seed is S-owned 0600 and not R-readable,
// and attest dir is R-writable (or at least present for R).
func AuditKeyLayout(keyDir, identity string, topo Topology) error {
	keyPath := PrivateKeyPath(keyDir, identity)
	if err := auditKeyMaterialPath(keyPath, topo.SignerUID); err != nil {
		return err
	}
	// Requester (and any non-S non-root) must not read seed.
	uid := os.Getuid()
	if uid != topo.SignerUID && uid != 0 {
		if data, err := os.ReadFile(keyPath); err == nil && len(data) > 0 {
			return fmt.Errorf("%w: private seed readable by current uid %d", ErrAdversarialSuccess, uid)
		}
	}
	attestDir := filepath.Join(keyDir, AttestSubdir)
	fi, err := os.Lstat(attestDir)
	if err != nil {
		return fmt.Errorf("%w: attest dir: %v", ErrProvisioning, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("%w: attest path not a directory", ErrProvisioning)
	}
	// Attest dir must not be world-writable.
	if fi.Mode().Perm()&0o002 != 0 {
		return fmt.Errorf("%w: attest dir world-writable", ErrKeyExposed)
	}
	return nil
}

// chownPath is a thin wrapper for tests.
func chownPath(path string, uid, gid int) error {
	return os.Chown(path, uid, gid)
}

// ownerUID of path if available.
func ownerUID(path string) (int, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("no stat_t")
	}
	return int(st.Uid), nil
}
