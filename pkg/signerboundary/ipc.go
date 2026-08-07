package signerboundary

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
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
