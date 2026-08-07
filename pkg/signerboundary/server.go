package signerboundary

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Server is the separate-uid signer. Peer authorization is kernel UID only
// (HERD_REQUESTER_UID), not executable path.
type Server struct {
	priv       ed25519.PrivateKey
	pub        ed25519.PublicKey
	sessionKey SessionKey
	topo       Topology
	socketPath string
	nonces     *DurableNonceLedger
	ln         net.Listener
	admit      AdmissionFunc
	// testPeerUID, if non-nil, overrides peerCreds (unit tests only).
	testPeerUID *int
}

// ServeOptions configures the signer daemon.
type ServeOptions struct {
	KeyPath         string
	SocketPath      string
	SessionKey      SessionKey
	Topology        Topology // must have three distinct UIDs + SocketGID
	NonceLedgerPath string
	// AdmissionLedgerPath is the durable FAC-145 grant file re-read on each sign.
	// Production serve requires this (or explicit Admission that wraps a ledger).
	AdmissionLedgerPath string
	// RequireDurableAdmission is NOT read: enforcement comes from
	// AdmissionLedgerPath (or an injected Admission). Setting it alone does not
	// force ledger checks. Retained only because the CLI serve path passes it;
	// making it enforce (or removing it) is deliberately out of scope here.
	RequireDurableAdmission bool
	// Admission overrides process-wide SetAdmission for this server.
	// When nil and AdmissionLedgerPath set, uses DurableAdmissionLedger.Admit.
	Admission AdmissionFunc
	// TestPeerUIDOverride injects peer UID for protocol unit tests only.
	// Production must leave nil so SO_PEERCRED is used.
	TestPeerUIDOverride *int
	// SkipSocketACL is test-only when chown is impossible; production must apply ACL.
	SkipSocketACL bool
}

// StartServer loads the key as current process uid (must equal Topology.SignerUID).
func StartServer(opts ServeOptions) (*Server, error) {
	if opts.Topology.SignerUID == 0 && opts.Topology.RequesterUID == 0 {
		return nil, fmt.Errorf("%w: Topology required (signer/requester/builder UIDs)", ErrProvisioning)
	}
	if opts.Topology.SignerUID == opts.Topology.RequesterUID ||
		opts.Topology.SignerUID == opts.Topology.BuilderUID ||
		opts.Topology.RequesterUID == opts.Topology.BuilderUID {
		return nil, fmt.Errorf("%w: topology UIDs must be distinct", ErrUnsupportedPlatform)
	}
	if opts.Topology.SocketGID <= 0 && !opts.SkipSocketACL {
		return nil, fmt.Errorf("%w: Topology.SocketGID required for cross-UID socket ACL", ErrUnsupportedPlatform)
	}
	if os.Getuid() != opts.Topology.SignerUID {
		return nil, fmt.Errorf("%w: serve must run as HERD_SIGNER_UID %d (got %d)",
			ErrProvisioning, opts.Topology.SignerUID, os.Getuid())
	}
	if err := auditKeyMaterialPath(opts.KeyPath, opts.Topology.SignerUID); err != nil {
		return nil, fmt.Errorf("server key audit: %w", err)
	}
	f, err := openKeyVerified(opts.KeyPath, opts.Topology.SignerUID)
	if err != nil {
		return nil, fmt.Errorf("server key open: %w", err)
	}
	data, err := ioReadAllClose(f)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	for i := range data {
		data[i] = 0
	}
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("corrupt key seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	for i := range seed {
		seed[i] = 0
	}
	pub := priv.Public().(ed25519.PublicKey)

	if len(opts.SessionKey) < 16 {
		return nil, fmt.Errorf("session key required")
	}
	sock := opts.SocketPath
	if err := validateSocketPath(sock); err != nil {
		return nil, err
	}
	if err := os.Remove(sock); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("remove stale socket: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return nil, fmt.Errorf("socket dir: %w", err)
	}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return nil, err
	}
	if !opts.SkipSocketACL {
		if err := applySocketACL(sock, opts.Topology.SignerUID, opts.Topology.SocketGID); err != nil {
			_ = ln.Close()
			return nil, err
		}
	} else {
		// Unit tests only — still never 0600 (hides R connect failure).
		if err := os.Chmod(sock, 0o666); err != nil {
			_ = ln.Close()
			return nil, err
		}
	}

	ledgerPath := opts.NonceLedgerPath
	if ledgerPath == "" {
		ledgerPath = sock + ".nonces"
	}
	ledger, err := NewDurableNonceLedger(ledgerPath)
	if err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("nonce ledger: %w", err)
	}

	admit := opts.Admission
	if admit == nil && strings.TrimSpace(opts.AdmissionLedgerPath) != "" {
		dled, err := OpenAdmissionLedgerTopo(opts.AdmissionLedgerPath, opts.Topology)
		if err != nil {
			_ = ln.Close()
			return nil, fmt.Errorf("admission ledger: %w", err)
		}
		admit = dled.Admit
	}
	if admit == nil {
		// Unit tests only: structural default. Production serve CLI requires ledger.
		admit = currentAdmission()
	}

	return &Server{
		priv:        priv,
		pub:         pub,
		sessionKey:  append(SessionKey(nil), opts.SessionKey...),
		topo:        opts.Topology,
		socketPath:  sock,
		nonces:      ledger,
		ln:          ln,
		admit:       admit,
		testPeerUID: opts.TestPeerUIDOverride,
	}, nil
}

// PublicKey returns the verification key.
func (s *Server) PublicKey() ed25519.PublicKey {
	if s == nil {
		return nil
	}
	out := make(ed25519.PublicKey, len(s.pub))
	copy(out, s.pub)
	return out
}

// Run accepts forever.
func (s *Server) Run() error {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return err
		}
		go s.handle(conn)
	}
}

// Close wipes secrets and closes the listener.
func (s *Server) Close() error {
	var first error
	if s.ln != nil {
		if err := s.ln.Close(); err != nil {
			first = err
		}
	}
	for i := range s.priv {
		s.priv[i] = 0
	}
	for i := range s.sessionKey {
		s.sessionKey[i] = 0
	}
	return first
}

// PID returns the server process id.
func (s *Server) PID() int { return os.Getpid() }

func (s *Server) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	// Read the full request first so peer-denial responses are not lost to a
	// write/close race with the client (broken pipe before ErrorCode is read).
	dec := json.NewDecoder(io.LimitReader(conn, MaxWireFrameBytes))
	var wr wireReq
	if err := dec.Decode(&wr); err != nil {
		return
	}
	req := wr.SignRequest
	if req.PayloadHex != "" && len(req.Payload) == 0 {
		if raw, err := hex.DecodeString(req.PayloadHex); err == nil {
			req.Payload = raw
		}
	}

	peerUID, peerPID, _, err := s.resolvePeer(conn)
	if err != nil {
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeUnauthorizedPeer, Error: "peer creds: " + err.Error(),
			EchoNonce: req.Nonce, PID: s.PID(),
		})
		return
	}
	// Kernel peer UID is the only authorization identity (not exe/role/env).
	if err := AuthorizePeerUID(peerUID, s.topo); err != nil {
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeUnauthorizedPeer, Error: err.Error(),
			EchoNonce: req.Nonce, PID: s.PID(),
		})
		return
	}
	if err := req.ValidateProduction(); err != nil {
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeInvalidRequest, Error: err.Error(), EchoNonce: req.Nonce, PID: s.PID(),
		})
		return
	}
	if !s.sessionKey.CheckRequestMAC(req, wr.MAC) {
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeUnauthorizedMAC, Error: ErrPeerUnauthorized.Error(), EchoNonce: req.Nonce, PID: s.PID(),
		})
		return
	}
	// FAC-145 admission hook: structurally valid is not sufficient.
	if req.Op == OpSignVerdict || req.Op == OpSignReceipt {
		admit := s.admit
		if admit == nil {
			admit = currentAdmission()
		}
		if admit != nil {
			if err := admit(req); err != nil {
				_ = json.NewEncoder(conn).Encode(wireResp{
					OK: false, ErrorCode: ErrCodeNotAdmitted, Error: err.Error(), EchoNonce: req.Nonce, PID: s.PID(),
				})
				return
			}
		}
	}
	if req.Op != OpPing && req.Op != OpProbe && !s.nonces.Accept(req.Nonce) {
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeReplay, Error: "nonce replay rejected", EchoNonce: req.Nonce, PID: s.PID(),
		})
		return
	}

	switch req.Op {
	case OpPing, OpProbe:
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: true, EchoNonce: req.Nonce, PID: s.PID(), PubKey: hex.EncodeToString(s.pub),
		})
	case OpExportKey:
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeExportRefused, Error: RefuseExport().Error(), EchoNonce: req.Nonce, PID: s.PID(),
		})
	case OpSignVerdict, OpSignReceipt:
		sig, err := SignWithKey(s.priv, req.Canonical())
		if err != nil {
			_ = json.NewEncoder(conn).Encode(wireResp{
				OK: false, ErrorCode: ErrCodeInvalidRequest, Error: err.Error(), EchoNonce: req.Nonce, PID: s.PID(),
			})
			return
		}
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: true, Signature: hex.EncodeToString(sig), EchoNonce: req.Nonce,
			PubKey: hex.EncodeToString(s.pub), PID: s.PID(),
		})
	default:
		_ = json.NewEncoder(conn).Encode(wireResp{
			OK: false, ErrorCode: ErrCodeUnknownOp, Error: "unknown op", EchoNonce: req.Nonce, PID: s.PID(),
		})
	}
	_ = peerPID
}

func (s *Server) resolvePeer(conn net.Conn) (uid, pid int, exe string, err error) {
	if s.testPeerUID != nil {
		return *s.testPeerUID, 1, "", nil
	}
	return peerCreds(conn)
}

func validateSocketPath(sock string) error {
	if strings.TrimSpace(sock) == "" {
		return fmt.Errorf("empty socket path")
	}
	if len(sock) > 100 {
		return fmt.Errorf("socket path too long for sockaddr_un")
	}
	if strings.Contains(sock, "\x00") {
		return fmt.Errorf("socket path contains NUL")
	}
	return nil
}
