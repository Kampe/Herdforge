package signerboundary

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// SealedSessionFile is the durable R-only session key path (FAC-169 handoff).
// Mode 0600, owner MUST be HERD_REQUESTER_UID. BuilderUID must differ so B
// cannot read it. Not session.mac (forbidden same-UID name).
const SealedSessionFile = "session.rkey"

// SealedSessionPath returns $keyDir/attest/session.rkey
func SealedSessionPath(keyDir string) string {
	return filepath.Join(keyDir, AttestSubdir, SealedSessionFile)
}

// WriteSealedSession writes the session key for repeated R use (not one-shot FD).
// Ownership: RequesterUID:SocketGID mode 0600. Fails if R==B.
func WriteSealedSession(keyDir string, topo Topology, sk SessionKey) error {
	if topo.RequesterUID == topo.BuilderUID {
		return fmt.Errorf("%w: cannot seal session when R==B", ErrUnsupportedPlatform)
	}
	if len(sk) < 16 {
		return fmt.Errorf("%w: session key too short", ErrProvisioning)
	}
	if err := os.MkdirAll(filepath.Join(keyDir, AttestSubdir), 0o770); err != nil {
		return err
	}
	path := SealedSessionPath(keyDir)
	line := []byte(hex.EncodeToString(sk) + "\n")
	// Write as current uid then chown to R.
	if err := atomicWriteFile(path, line, 0o600); err != nil {
		return err
	}
	if err := os.Chown(path, topo.RequesterUID, topo.SocketGID); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("%w: chown sealed session to R=%d: %v (need root launcher or run as R)",
			ErrProvisioning, topo.RequesterUID, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return auditSealedSession(path, topo.RequesterUID)
}

// LoadSealedSession loads session.rkey only if owned by current uid (R) and 0600.
func LoadSealedSession(keyDir string) (SessionKey, error) {
	path := SealedSessionPath(keyDir)
	if err := auditSealedSession(path, os.Getuid()); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("%w: read sealed session: %v", ErrProvisioning, err)
	}
	return decodeSessionKeyHex(string(data))
}

func auditSealedSession(path string, wantUID int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: sealed session: %v", ErrBoundaryNotEstablished, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: sealed session is symlink", ErrKeyExposed)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: sealed session not regular file", ErrKeyExposed)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: sealed session mode %04o not 0600", ErrKeyExposed, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot stat sealed session owner", ErrProvisioning)
	}
	if int(st.Uid) != wantUID {
		return fmt.Errorf("%w: sealed session owner uid %d want %d", ErrProvisioning, st.Uid, wantUID)
	}
	return nil
}

// loadCoordinatorSessionKey prefers sealed R-owned file for repeated use, then FD/stdin.
func loadCoordinatorSessionKey() (SessionKey, error) {
	if dir := os.Getenv(KeyDirEnv); dir != "" {
		if sk, err := LoadSealedSession(dir); err == nil {
			return sk, nil
		}
	}
	if dir, err := ResolveKeyDir(); err == nil {
		if sk, err := LoadSealedSession(dir); err == nil {
			return sk, nil
		}
	}
	if fdStr := os.Getenv("HERD_SIGNER_SESSION_KEY_FD"); fdStr != "" {
		return loadSessionKeyFromFD(fdStr)
	}
	if os.Getenv("HERD_SIGNER_SESSION_STDIN") == "1" {
		return loadSessionKeyFromReader(os.Stdin)
	}
	if os.Getenv("HERD_SIGNER_INSECURE_ENV_SESSION") == "1" {
		hexKey := os.Getenv(EnvSessionKey)
		if hexKey == "" {
			return nil, fmt.Errorf("%w: INSECURE mode but %s empty", ErrProvisioning, EnvSessionKey)
		}
		return decodeSessionKeyHex(hexKey)
	}
	return nil, fmt.Errorf("%w: no session key — sealed session.rkey (R-owned 0600), HERD_SIGNER_SESSION_KEY_FD, or SESSION_STDIN required",
		ErrUnsupportedPlatform)
}
