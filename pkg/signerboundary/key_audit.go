package signerboundary

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const keyAuditDomain = "herdforge-signer-audit-key-v1"

// KeyAuditStatement is produced only by the signer after a fresh local audit
// of its configured key. Clients must not supply these fields.
type KeyAuditStatement struct {
	Domain   string `json:"domain"`
	Nonce    string `json:"nonce"`
	Path     string `json:"path"`
	Identity string `json:"identity"`
	OwnerUID int    `json:"owner_uid"`
	Mode     string `json:"mode"`
	Nlink    uint64 `json:"nlink"`
	ServerPID int   `json:"server_pid"`
	ServerUID int   `json:"server_uid"`
}

func (s *Server) handleKeyAudit(req SignRequest) (KeyAuditStatement, []byte, error) {
	if s == nil || strings.TrimSpace(s.keyPath) == "" || strings.TrimSpace(s.identity) == "" {
		return KeyAuditStatement{}, nil, fmt.Errorf("%w: server has no bound key identity", ErrProvisioning)
	}
	if len(req.payloadBytes()) != 0 {
		return KeyAuditStatement{}, nil, fmt.Errorf("%w: client audit facts refused", ErrPeerUnauthorized)
	}
	if err := auditKeyMaterialPath(s.keyPath, s.topo.SignerUID); err != nil {
		return KeyAuditStatement{}, nil, err
	}
	fi, err := os.Lstat(s.keyPath)
	if err != nil {
		return KeyAuditStatement{}, nil, fmt.Errorf("%w: re-stat key: %v", ErrProvisioning, err)
	}
	owner, ok := statUID(fi)
	if !ok {
		return KeyAuditStatement{}, nil, fmt.Errorf("%w: key owner unreadable", ErrProvisioning)
	}
	nlink, _ := statNlink(fi)
	st := KeyAuditStatement{
		Domain:    keyAuditDomain,
		Nonce:     req.Nonce,
		Path:      s.keyPath,
		Identity:  s.identity,
		OwnerUID:  owner,
		Mode:      fmt.Sprintf("%#o", fi.Mode().Perm()),
		Nlink:     nlink,
		ServerPID: s.PID(),
		ServerUID: os.Getuid(),
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return KeyAuditStatement{}, nil, err
	}
	signReq := req
	signReq.Payload = raw
	signReq.PayloadHex = hex.EncodeToString(raw)
	sig, err := SignWithKey(s.priv, signReq.Canonical())
	if err != nil {
		return KeyAuditStatement{}, nil, err
	}
	return st, sig, nil
}

// AuditKeyAsRequester is the requester CLI/lab entry: session-authenticated
// audit-key IPC plus published-key verification. Never inspects private/.
func AuditKeyAsRequester(opts Options, nonce string) error {
	topo, err := RequireTopology()
	if err != nil {
		return err
	}
	keyPath := PrivateKeyPath(opts.KeyDir, opts.Identity)
	pub, err := loadPublishedPublicKey(opts.RepoRoot)
	if err != nil {
		return fmt.Errorf("%w: published public key required: %v", ErrProvisioning, err)
	}
	sock := strings.TrimSpace(os.Getenv(EnvSignerSock))
	if sock == "" {
		return fmt.Errorf("%w: %s required", ErrProvisioning, EnvSignerSock)
	}
	sk, err := loadOrCreateSessionKey(opts.KeyDir)
	if err != nil {
		return err
	}
	req := SignRequest{Op: OpKeyAudit, SessionID: "audit-key-cli", Nonce: nonce}
	st, sig, wirePID, wirePub, err := requestKeyAuditOverIPC(sock, sk, &req)
	if err != nil {
		return err
	}
	if len(pub) == 0 {
		pub = wirePub
	}
	if err := verifyKeyAudit(pub, keyPath, opts.Identity, topo.SignerUID, 0, req, st, sig); err != nil {
		return err
	}
	if wirePID != 0 && wirePID != st.ServerPID {
		return fmt.Errorf("%w: audit wire pid mismatch", ErrProvisioning)
	}
	return nil
}

func verifyKeyAudit(pub ed25519.PublicKey, expectedPath, expectedIdentity string, expectedUID, expectedPID int, req SignRequest, st KeyAuditStatement, sig []byte) error {
	if strings.TrimSpace(st.Domain) != keyAuditDomain {
		return fmt.Errorf("%w: audit domain mismatch", ErrProvisioning)
	}
	if st.Nonce != req.Nonce || strings.TrimSpace(st.Nonce) == "" {
		return fmt.Errorf("%w: audit nonce mismatch", ErrProvisioning)
	}
	if st.Path != expectedPath {
		return fmt.Errorf("%w: audit path mismatch", ErrProvisioning)
	}
	if st.Identity != expectedIdentity {
		return fmt.Errorf("%w: audit identity mismatch", ErrProvisioning)
	}
	if st.OwnerUID != expectedUID || st.ServerUID != expectedUID {
		return fmt.Errorf("%w: audit uid mismatch", ErrProvisioning)
	}
	if expectedPID > 0 && st.ServerPID != expectedPID {
		return fmt.Errorf("%w: audit server pid mismatch", ErrProvisioning)
	}
	if st.Mode != "0600" && st.Mode != "0o600" {
		return fmt.Errorf("%w: audit mode %s not 0600", ErrKeyExposed, st.Mode)
	}
	if st.Nlink != 0 && st.Nlink != 1 {
		return fmt.Errorf("%w: audit nlink=%d", ErrKeyExposed, st.Nlink)
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	vr := req
	vr.Payload = raw
	vr.PayloadHex = hex.EncodeToString(raw)
	if len(pub) != ed25519.PublicKeySize || len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("%w: audit signature size", ErrProvisioning)
	}
	if !ed25519.Verify(pub, vr.Canonical(), sig) {
		return fmt.Errorf("%w: audit signature rejected", ErrProvisioning)
	}
	return nil
}
