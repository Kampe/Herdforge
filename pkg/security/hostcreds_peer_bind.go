package security

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Client-port binding is the production-compatible exact-session worker
// authority for loopback MITM (no flaky lsof PID guessing).
//
// Protocol:
//  1. Worker claims an exclusive local TCP source port (bind 127.0.0.1:0, no listen).
//  2. Writes port to claim file; parent AllowClientPort(port); writes claim.ok.
//  3. Worker CONNECT/dials MITM from that exact LocalAddr port.
//  4. MITM authorizePeer requires RemoteAddr().Port ∈ allowedPorts (kernel-visible).
//
// On platforms without a kernel-exact peer-PID API (Darwin TCP), port claim is
// the only production peer attribution. PID-based allow is Linux /proc only.

// ClaimLocalPort binds 127.0.0.1:0 and returns the bound FD as *os.File plus port.
// Caller must ConnectClaimed or Close it. Port is exclusive until the FD closes.
func ClaimLocalPort() (port int, f *os.File, err error) {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	if err != nil {
		return 0, nil, err
	}
	// Do not set SO_REUSEADDR: exclusivity is the point of the claim.
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
// After success, returns a net.Conn; caller should Close the original claim file.
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
	conn, err := net.FileConn(f)
	if err != nil {
		return nil, err
	}
	return conn, nil
}

// WaitAllowAndDial claims a port, publishes it for parent AllowClientPort,
// waits for claim.ok, then dials the MITM from the claimed source port.
func WaitAllowAndDial(proxyURL, claimPath string, wait time.Duration) (net.Conn, int, error) {
	proxyHostPort := stripProxyURL(proxyURL)
	port, f, err := ClaimLocalPort()
	if err != nil {
		return nil, 0, err
	}
	if err := os.WriteFile(claimPath, []byte(strconv.Itoa(port)+"\n"), 0o600); err != nil {
		_ = f.Close()
		return nil, 0, err
	}
	okPath := claimPath + ".ok"
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(okPath); err == nil {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if _, err := os.Stat(okPath); err != nil {
		_ = f.Close()
		return nil, port, fmt.Errorf("allow handshake timeout")
	}
	c, err := ConnectClaimed(f, proxyHostPort)
	_ = f.Close()
	if err != nil {
		return nil, port, err
	}
	return c, port, nil
}

// ParentAllowClaimedPort polls claimPath, AllowClientPort on mitm, writes .ok.
func ParentAllowClaimedPort(mitm *TLSMitmProxy, claimPath string, wait time.Duration) (int, error) {
	if mitm == nil {
		return 0, fmt.Errorf("nil mitm")
	}
	deadline := time.Now().Add(wait)
	var port int
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(claimPath)
		if err == nil {
			p, perr := strconv.Atoi(strings.TrimSpace(string(raw)))
			if perr == nil && p > 0 {
				port = p
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	if port <= 0 {
		return 0, fmt.Errorf("claim port timeout")
	}
	mitm.AllowClientPort(port)
	if err := os.WriteFile(claimPath+".ok", []byte("ok\n"), 0o600); err != nil {
		return port, err
	}
	return port, nil
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
