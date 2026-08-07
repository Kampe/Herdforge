package signerboundary

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"time"
)

func dialForErrorCode(socketPath string, req SignRequest, mac string) (string, error) {
	if err := req.EnsureNonce(); err != nil {
		return "", err
	}
	if len(req.Payload) > 0 {
		req.PayloadHex = hex.EncodeToString(req.Payload)
	}
	conn, err := net.DialTimeout("unix", socketPath, 3*time.Second)
	if err != nil {
		return "", fmt.Errorf("dial signer: %w", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(wireReq{SignRequest: req, MAC: mac}); err != nil {
		return "", err
	}
	var resp wireResp
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return "", err
	}
	if resp.OK {
		return "", nil
	}
	if resp.ErrorCode == "" {
		return "", fmt.Errorf("signer: %s (missing error_code — not a structured denial)", resp.Error)
	}
	return resp.ErrorCode, fmt.Errorf("signer: %s", resp.Error)
}
