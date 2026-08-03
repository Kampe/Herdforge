package security

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PeerGrant is a single-use, connection-bound peer capability for MITM CONNECT.
//
// Production registration: parent ClaimLocalPort, AllowOneShotPeer(grant), start
// child with ExtraFiles=[claimFD]. Child dials MITM from that FD. authorizePeer
// matches RemoteAddr().Port and CONSUMES the grant (replay denied).
//
// No claim-file handshake: a same-UID adversary can forge a port number on disk.
// Kernel ownership of the bound FD is the attribution root until FAC-169 IPC.
type PeerGrant struct {
	Port            int
	SessionID       string
	CapabilityNonce string
	AuthorPID       int // optional; 0 if unknown at grant time
	Consumed        bool
}

// ClaimLocalPort binds 127.0.0.1:0 (no listen) and returns exclusive source port + FD.
func ClaimLocalPort() (port int, f *os.File, err error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0, nil, err
	}
	sa := &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return 0, nil, err
	}
	rsa, err := syscall.Getsockname(fd)
	if err != nil {
		_ = syscall.Close(fd)
		return 0, nil, err
	}
	ina, ok := rsa.(*syscall.SockaddrInet4)
	if !ok {
		_ = syscall.Close(fd)
		return 0, nil, fmt.Errorf("unexpected sockaddr")
	}
	port = ina.Port
	if port <= 0 {
		_ = syscall.Close(fd)
		return 0, nil, fmt.Errorf("invalid claim port")
	}
	f = os.NewFile(uintptr(fd), "hc-claim")
	return port, f, nil
}

// ConnectClaimed dials proxyAddr (host:port) using an already-bound claim FD.
func ConnectClaimed(f *os.File, proxyAddr string) (net.Conn, error) {
	if f == nil {
		return nil, fmt.Errorf("nil claim fd")
	}
	host, portStr, err := net.SplitHostPort(proxyAddr)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(portStr)
	if err != nil || p <= 0 {
		return nil, fmt.Errorf("proxy port")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return nil, fmt.Errorf("proxy host not an IP")
	}
	ip4 := ip.To4()
	if ip4 == nil {
		return nil, fmt.Errorf("need ipv4 proxy")
	}
	var fdErr error
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}
	err = raw.Control(func(fd uintptr) {
		addr := &syscall.SockaddrInet4{Port: p, Addr: [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}}
		if e := syscall.Connect(int(fd), addr); e != nil {
			fdErr = e
		}
	})
	if err != nil {
		return nil, err
	}
	if fdErr != nil {
		return nil, fdErr
	}
	return net.FileConn(f)
}

// OpenClaimFDFromEnv opens the inherited claim FD (ExtraFiles → usually 3).
func OpenClaimFDFromEnv() (*os.File, error) {
	raw := os.Getenv("HERD_HOSTCREDS_CLAIM_FD")
	if raw == "" {
		return nil, fmt.Errorf("HERD_HOSTCREDS_CLAIM_FD unset")
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 3 {
		return nil, fmt.Errorf("bad claim fd")
	}
	fd, err := syscall.Dup(n)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), "hc-claim-inherited"), nil
}

// ConnectViaInheritedClaim dials MITM using the inherited one-shot claim FD.
func ConnectViaInheritedClaim(proxyURL string) (net.Conn, int, error) {
	f, err := OpenClaimFDFromEnv()
	if err != nil {
		return nil, 0, err
	}
	port, err := localPortOfFile(f)
	if err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	c, err := ConnectClaimed(f, stripProxyURL(proxyURL))
	_ = f.Close()
	if err != nil {
		return nil, port, err
	}
	return c, port, nil
}

func localPortOfFile(f *os.File) (int, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return 0, err
	}
	var port int
	var ferr error
	err = raw.Control(func(fd uintptr) {
		sa, e := syscall.Getsockname(int(fd))
		if e != nil {
			ferr = e
			return
		}
		ina, ok := sa.(*syscall.SockaddrInet4)
		if !ok {
			ferr = fmt.Errorf("sockaddr")
			return
		}
		port = ina.Port
	})
	if err != nil {
		return 0, err
	}
	return port, ferr
}

func stripProxyURL(proxyURL string) string {
	proxyURL = strings.TrimSpace(proxyURL)
	proxyURL = strings.TrimPrefix(proxyURL, "http://")
	proxyURL = strings.TrimPrefix(proxyURL, "https://")
	if i := strings.IndexByte(proxyURL, '/'); i >= 0 {
		proxyURL = proxyURL[:i]
	}
	return proxyURL
}

// RequestDigest is a non-secret SHA-256 hex of session|method|host|path|nonce|bodyCap.
func RequestDigest(sessionID, method, host, path, nonce, bodyPrefix string) string {
	sum := sha256.Sum256([]byte(sessionID + "|" + method + "|" + host + "|" + path + "|" + nonce + "|" + bodyPrefix))
	return hex.EncodeToString(sum[:])
}

// PeerBindingMAC coordinator-side binding (not production authority alone).
func PeerBindingMAC(key []byte, sessionID string, port, pid int, nonce string) string {
	mac := hmac.New(sha256.New, key)
	_, _ = fmt.Fprintf(mac, "%s|%d|%d|%s", sessionID, port, pid, nonce)
	return hex.EncodeToString(mac.Sum(nil))
}

func waitUntil(d time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fn()
}

// bindExactPort attempts to bind 127.0.0.1:port exclusively (replay tests).
func bindExactPort(port int) (int, error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0, err
	}
	sa := &syscall.SockaddrInet4{Port: port, Addr: [4]byte{127, 0, 0, 1}}
	if err := syscall.Bind(fd, sa); err != nil {
		_ = syscall.Close(fd)
		return 0, err
	}
	return fd, nil
}
