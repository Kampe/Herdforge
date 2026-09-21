package signerboundary

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"runtime"
	"strings"
	"time"
)

// Wire protocol for authenticated SignRequest IPC.

type wireReq struct {
	SignRequest
	MAC string `json:"mac"`
}

// Wire error codes — clients must match these, not fuzzy substrings.
const (
	ErrCodeUnauthorizedMAC  = "UNAUTHORIZED_MAC"
	ErrCodeUnauthorizedPeer = "UNAUTHORIZED_PEER"
	ErrCodeReplay           = "NONCE_REPLAY"
	ErrCodeInvalidRequest   = "INVALID_REQUEST"
	ErrCodeExportRefused    = "EXPORT_REFUSED"
	ErrCodeUnknownOp        = "UNKNOWN_OP"
	ErrCodeNotAdmitted      = "NOT_ADMITTED"
)

type wireResp struct {
	OK        bool   `json:"ok"`
	Error     string `json:"error,omitempty"`
	ErrorCode string `json:"error_code,omitempty"`
	Signature string `json:"signature,omitempty"`
	EchoNonce string `json:"echo_nonce"`
	PubKey    string `json:"public_key,omitempty"`
	PID       int    `json:"pid,omitempty"`
	// Audit is server-produced canonical audit-key statement JSON. Never client-supplied.
	Audit string `json:"audit,omitempty"`
	// SignerBinding is ed25519(sig over probe/attestation material) when relevant.
	SignerBinding string        `json:"signer_binding,omitempty"`
	Receipt       *ProbeReceipt `json:"receipt,omitempty"`
}

// signRequestOverIPC takes a pointer so EnsureNonce mutates the caller's
// request — required for exact-request anti-replay tests (FAC-169 §a).
func signRequestOverIPC(socketPath string, key SessionKey, req *SignRequest) ([]byte, error) {
	if req == nil {
		return nil, fmt.Errorf("nil SignRequest")
	}
	if err := req.ValidateProduction(); err != nil {
		return nil, err
	}
	if err := req.EnsureNonce(); err != nil {
		return nil, err
	}
	if len(req.Payload) > 0 {
		req.PayloadHex = hex.EncodeToString(req.Payload)
	}
	mac := key.BindRequestMAC(*req)
	return signRequestOverIPCWithMAC(socketPath, *req, mac)
}

func signRequestOverIPCWithMAC(socketPath string, req SignRequest, mac string) ([]byte, error) {
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial signer: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))

	wr := wireReq{SignRequest: req, MAC: mac}
	if err := json.NewEncoder(conn).Encode(wr); err != nil {
		return nil, err
	}
	var resp wireResp
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, err
	}
	if !resp.OK {
		return nil, fmt.Errorf("signer: %s", resp.Error)
	}
	if resp.EchoNonce != req.Nonce {
		return nil, fmt.Errorf("signer: nonce binding failed")
	}
	if req.Op == OpPing || req.Op == OpProbe {
		return nil, nil
	}
	sig, err := hex.DecodeString(resp.Signature)
	if err != nil {
		return nil, err
	}
	return sig, nil
}

func requestKeyAuditOverIPC(socketPath string, key SessionKey, expectedUID int, req *SignRequest) (KeyAuditStatement, []byte, int, error) {
	var zero KeyAuditStatement
	if req == nil {
		return zero, nil, 0, fmt.Errorf("nil SignRequest")
	}
	if expectedUID <= 0 {
		return zero, nil, 0, fmt.Errorf("%w: expected signer uid required before session MAC", ErrProvisioning)
	}
	req.Op = OpKeyAudit
	req.Payload = nil
	req.PayloadHex = ""
	if err := req.ValidateProduction(); err != nil {
		return zero, nil, 0, err
	}
	if err := req.EnsureNonce(); err != nil {
		return zero, nil, 0, err
	}
	mac := key.BindRequestMAC(*req)
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return zero, nil, 0, fmt.Errorf("dial signer: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	peerUID, peerPID, _, err := peerCreds(conn)
	if err != nil {
		return zero, nil, 0, fmt.Errorf("%w: kernel peer creds before MAC: %v", ErrProvisioning, err)
	}
	if peerUID != expectedUID {
		return zero, nil, peerPID, fmt.Errorf("%w: kernel peer uid %d want signer %d", ErrPeerUnauthorized, peerUID, expectedUID)
	}
	if runtime.GOOS == "linux" && peerPID <= 0 {
		return zero, nil, peerPID, fmt.Errorf("%w: linux kernel peer pid required", ErrProvisioning)
	}
	if err := json.NewEncoder(conn).Encode(wireReq{SignRequest: *req, MAC: mac}); err != nil {
		return zero, nil, peerPID, err
	}
	dec := json.NewDecoder(io.LimitReader(conn, MaxWireFrameBytes))
	var resp wireResp
	if err := dec.Decode(&resp); err != nil {
		return zero, nil, peerPID, err
	}
	if resp.OK && (strings.TrimSpace(resp.Error) != "" || strings.TrimSpace(resp.ErrorCode) != "") {
		return zero, nil, peerPID, fmt.Errorf("%w: ok response carried error fields", ErrProvisioning)
	}
	if !resp.OK {
		return zero, nil, peerPID, fmt.Errorf("signer: %s", resp.Error)
	}
	if resp.EchoNonce != req.Nonce {
		return zero, nil, peerPID, fmt.Errorf("signer: nonce binding failed")
	}
	if strings.TrimSpace(resp.Audit) == "" {
		return zero, nil, peerPID, fmt.Errorf("%w: empty audit statement", ErrProvisioning)
	}
	var st KeyAuditStatement
	aud := json.NewDecoder(strings.NewReader(resp.Audit))
	aud.DisallowUnknownFields()
	if err := aud.Decode(&st); err != nil {
		return zero, nil, peerPID, fmt.Errorf("%w: malformed or unknown audit fields: %v", ErrProvisioning, err)
	}
	sig, err := hex.DecodeString(resp.Signature)
	if err != nil {
		return zero, nil, peerPID, err
	}
	if runtime.GOOS == "linux" {
		if st.ServerPID <= 0 || st.ServerPID != peerPID {
			return zero, nil, peerPID, fmt.Errorf("%w: signed pid %d != kernel peer pid %d", ErrProvisioning, st.ServerPID, peerPID)
		}
	}
	return st, sig, peerPID, nil
}
