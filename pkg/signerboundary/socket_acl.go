package signerboundary

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// Socket ACL model (FAC-169 finding #3):
//
// Peer SO_PEERCRED is the authorization boundary. The socket must still be
// connectable by both R (legitimate) and B (adversary) so B receives
// UNAUTHORIZED_PEER rather than a vacuous dial failure, while private key
// material remains 0600 S-only.
//
// Design:
//   - Unix socket owned by S:SocketGID
//   - mode 0660 (group-write required to connect on Unix)
//   - SocketGID is a dedicated IPC group whose members include S, R, and B
//     (B may dial; peer UID still denies B). Non-members cannot dial.
//   - HERD_SIGNER_SOCK_GID required in production topology.
//
// chmod 0600 owner-only is FORBIDDEN: R could not connect.

const EnvSocketGID = "HERD_SIGNER_SOCK_GID"

// applySocketACL sets ownership and 0660 after Listen.
func applySocketACL(socketPath string, ownerUID, sockGID int) error {
	if sockGID <= 0 {
		return fmt.Errorf("%w: SocketGID required for cross-UID dial ACL (set %s)", ErrUnsupportedPlatform, EnvSocketGID)
	}
	if err := os.Chown(socketPath, ownerUID, sockGID); err != nil {
		return fmt.Errorf("%w: socket chown uid=%d gid=%d: %v", ErrProvisioning, ownerUID, sockGID, err)
	}
	// 0660: owner+group may connect; peer UID decides authorization.
	if err := os.Chmod(socketPath, 0o660); err != nil {
		return fmt.Errorf("%w: socket chmod 0660: %v", ErrProvisioning, err)
	}
	fi, err := os.Lstat(socketPath)
	if err != nil {
		return err
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%w: cannot stat socket for ACL verify", ErrProvisioning)
	}
	if int(st.Uid) != ownerUID || int(st.Gid) != sockGID {
		return fmt.Errorf("%w: socket ownership uid=%d gid=%d want uid=%d gid=%d",
			ErrProvisioning, st.Uid, st.Gid, ownerUID, sockGID)
	}
	if fi.Mode().Perm()&0o077 != 0o060 {
		// Expect exactly ug=rw, o=0 → 0660
		if fi.Mode().Perm() != 0o660 {
			return fmt.Errorf("%w: socket mode %04o want 0660", ErrProvisioning, fi.Mode().Perm())
		}
	}
	return nil
}

func parseSocketGID() (int, error) {
	raw := strings.TrimSpace(os.Getenv(EnvSocketGID))
	if raw == "" {
		return 0, fmt.Errorf("%w: %s required (IPC group for S/R/B dial; peer UID still authorizes only R)",
			ErrUnsupportedPlatform, EnvSocketGID)
	}
	g, err := strconv.Atoi(raw)
	if err != nil || g <= 0 {
		return 0, fmt.Errorf("%w: invalid %s=%q", ErrProvisioning, EnvSocketGID, raw)
	}
	return g, nil
}
