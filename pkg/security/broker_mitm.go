package security

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// brokerCA is an ephemeral CA for coordinator credential MITM on CONNECT
// to HostCreds hosts. The CA certificate is public (agent trusts it via
// SSL_CERT_FILE); private key and HostCreds never leave the broker process.
type brokerCA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte
	mu      sync.Mutex
	leaves  map[string]*tls.Certificate
}

func newBrokerCA() (*brokerCA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "herd-broker-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return &brokerCA{cert: cert, key: key, certPEM: pemBytes, leaves: map[string]*tls.Certificate{}}, nil
}

func (c *brokerCA) leafFor(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
	}
	c.leaves[host] = leaf
	return leaf, nil
}

// dialAllowedCredentialed performs CONNECT MITM for hosts with HostCreds:
// terminate TLS from agent (agent trusts broker CA), inject Authorization,
// open TLS to upstream. HostCreds never appear in agent env.
func (b *HostAllowBroker) dialAllowedCredentialed(client net.Conn, host, port, authorization string) error {
	if b.ca == nil {
		ca, err := newBrokerCA()
		if err != nil {
			return err
		}
		b.ca = ca
	}
	leaf, err := b.ca.leafFor(host)
	if err != nil {
		return err
	}
	ip, err := resolveAndPinIP(host)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\n\r\n")
		return err
	}
	upRaw, err := net.DialTimeout("tcp", net.JoinHostPort(ip.String(), port), 5*time.Second)
	if err != nil {
		_, _ = io.WriteString(client, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n")
		return err
	}
	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		_ = upRaw.Close()
		return err
	}
	_ = client.SetDeadline(time.Time{})

	clientTLS := tls.Server(client, &tls.Config{
		Certificates: []tls.Certificate{*leaf},
		MinVersion:   tls.VersionTLS12,
	})
	if err := clientTLS.Handshake(); err != nil {
		_ = upRaw.Close()
		return err
	}
	upTLS := tls.Client(upRaw, &tls.Config{
		ServerName: host,
		MinVersion: tls.VersionTLS12,
	})
	if err := upTLS.Handshake(); err != nil {
		_ = clientTLS.Close()
		_ = upRaw.Close()
		return err
	}

	// First request: inject Authorization if missing.
	br := bufio.NewReader(clientTLS)
	req, err := http.ReadRequest(br)
	if err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return err
	}
	if req.Header.Get("Authorization") == "" && authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	req.RequestURI = ""
	if err := req.Write(upTLS); err != nil {
		_ = clientTLS.Close()
		_ = upTLS.Close()
		return err
	}
	// Pipe remainder (including response + further requests).
	// Remaining buffered client bytes:
	if n := br.Buffered(); n > 0 {
		buf := make([]byte, n)
		_, _ = br.Read(buf)
		_, _ = upTLS.Write(buf)
	}
	pipe(clientTLS, upTLS)
	return nil
}

// CAPEM returns the public CA certificate PEM for agent trust (not secret).
func (b *HostAllowBroker) CAPEM() []byte {
	if b == nil || b.ca == nil {
		return nil
	}
	return append([]byte(nil), b.ca.certPEM...)
}

// EnsureCA initializes the ephemeral CA if HostCreds will be used.
func (b *HostAllowBroker) EnsureCA() error {
	if b == nil {
		return fmt.Errorf("nil broker")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.ca != nil {
		return nil
	}
	ca, err := newBrokerCA()
	if err != nil {
		return err
	}
	b.ca = ca
	return nil
}

// hostCred returns coordinator-held Authorization for host (case-insensitive).
func (b *HostAllowBroker) hostCred(host string) string {
	if b == nil {
		return ""
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.HostCreds == nil {
		return ""
	}
	return b.HostCreds[strings.ToLower(host)]
}
