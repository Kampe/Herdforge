//go:build !linux && !darwin

package security

import "net"

func localPeerPID(c net.Conn) int {
	_ = c
	return 0
}
