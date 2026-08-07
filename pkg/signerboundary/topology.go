package signerboundary

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Kernel-enforced identity topology (FAC-169):
//
//	HERD_SIGNER_UID      S  — owns private key; runs serve
//	HERD_REQUESTER_UID   R  — only UID allowed to request signatures (SO_PEERCRED)
//	HERD_BUILDER_UID     B  — builder/worker; must differ from R
//	HERD_SIGNER_SOCK_GID G  — IPC group on the unix socket (0660 S:G); members
//	                          include S, R, and B so B can dial and receive
//	                          UNAUTHORIZED_PEER (not EACCES theater)
//
// FD/stdin secrets are not an OS boundary under same-UID attach. Peer-cred
// authorization is only meaningful when R != B at the kernel UID layer.
// Exe path, HERD_ROLE, and env scrubbing are NOT authority.

const (
	EnvRequesterUID = "HERD_REQUESTER_UID"
	EnvBuilderUID   = "HERD_BUILDER_UID"
)

// Topology is the OS identity set required for separate-uid acceptance.
type Topology struct {
	SignerUID    int
	RequesterUID int
	BuilderUID   int
	SocketGID    int // dedicated IPC group for socket 0660
}

// LoadTopology reads distinct S/R/B and required SocketGID without requiring
// the current process to be the requester (used by serve as S and the launcher).
func LoadTopology() (Topology, error) {
	s, err := parseUIDEnv(EnvSignerUID, true)
	if err != nil {
		return Topology{}, err
	}
	r, err := parseUIDEnv(EnvRequesterUID, true)
	if err != nil {
		return Topology{}, err
	}
	b, err := parseUIDEnv(EnvBuilderUID, true)
	if err != nil {
		return Topology{}, err
	}
	if s == r || s == b || r == b {
		return Topology{}, fmt.Errorf("%w: signer/requester/builder UIDs must be three distinct kernel identities (got S=%d R=%d B=%d) — same-UID request authority is theater",
			ErrUnsupportedPlatform, s, r, b)
	}
	g, err := parseSocketGID()
	if err != nil {
		return Topology{}, err
	}
	return Topology{SignerUID: s, RequesterUID: r, BuilderUID: b, SocketGID: g}, nil
}

// RequireTopology loads topology and requires the current process is the requester (R).
func RequireTopology() (Topology, error) {
	topo, err := LoadTopology()
	if err != nil {
		return Topology{}, err
	}
	if os.Getuid() != topo.RequesterUID {
		return Topology{}, fmt.Errorf("%w: process uid %d != HERD_REQUESTER_UID %d — coordinator must run as requester identity",
			ErrProvisioning, os.Getuid(), topo.RequesterUID)
	}
	return topo, nil
}

func parseUIDEnv(name string, required bool) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		if required {
			return 0, fmt.Errorf("%w: %s required for kernel peer-cred topology", ErrUnsupportedPlatform, name)
		}
		return -1, nil
	}
	u, err := strconv.Atoi(raw)
	if err != nil || u < 0 {
		return 0, fmt.Errorf("%w: invalid %s=%q", ErrProvisioning, name, raw)
	}
	return u, nil
}

// AuthorizePeerUID is the sole peer-authorization check: kernel peer UID must
// equal the provisioned requester and must not be the signer or builder.
func AuthorizePeerUID(peerUID int, topo Topology) error {
	if peerUID == topo.SignerUID {
		return fmt.Errorf("%w: peer is signer uid", ErrPeerUnauthorized)
	}
	if peerUID == topo.BuilderUID {
		return fmt.Errorf("%w: peer is builder uid — workers cannot request signatures", ErrPeerUnauthorized)
	}
	if peerUID != topo.RequesterUID {
		return fmt.Errorf("%w: peer uid %d is not HERD_REQUESTER_UID %d", ErrPeerUnauthorized, peerUID, topo.RequesterUID)
	}
	return nil
}
