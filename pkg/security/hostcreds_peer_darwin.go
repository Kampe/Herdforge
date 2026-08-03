//go:build darwin

package security

import "net"

// localPeerPID is intentionally unsupported on Darwin for production peer
// attribution. lsof -iTCP:<port> matches both endpoints and unrelated sockets
// and is nondeterministic (last PID wins) — not authoritative.
//
// Production peer binding uses exclusive client-port claim (AllowClientPort /
// RemoteAddr().Port). Returning 0 fails closed for PID-based allow.
func localPeerPID(c net.Conn) int {
	_ = c
	return 0
}

// peerPIDSupported reports whether kernel-exact peer PID is available.
func peerPIDSupported() bool { return false }
