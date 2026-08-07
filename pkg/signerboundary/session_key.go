package signerboundary

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Session MAC handling (integrity binding only — NOT the OS boundary).
//
// OS isolation requires Topology R != B. Session material for R lives in:
//   - sealed session.rkey (0600 owner R) for repeated coordinator use
//   - FD/stdin one-shot handoff during bootstrap
//
// Never: session.mac, bare env, argv hex.

// loadSessionKeyFromFD reads one hex line from an open FD.
func loadSessionKeyFromFD(fdStr string) (SessionKey, error) {
	fd, err := strconv.Atoi(fdStr)
	if err != nil || fd < 0 {
		return nil, fmt.Errorf("%w: bad HERD_SIGNER_SESSION_KEY_FD", ErrProvisioning)
	}
	f := os.NewFile(uintptr(fd), "session-key-fd")
	if f == nil {
		return nil, fmt.Errorf("%w: invalid session key fd", ErrProvisioning)
	}
	return loadSessionKeyFromReader(f)
}

func loadSessionKeyFromReader(r io.Reader) (SessionKey, error) {
	br := bufio.NewReader(r)
	line, err := br.ReadString('\n')
	if err != nil && len(line) == 0 {
		return nil, fmt.Errorf("%w: read session key: %v", ErrProvisioning, err)
	}
	return decodeSessionKeyHex(strings.TrimSpace(line))
}

func decodeSessionKeyHex(hexKey string) (SessionKey, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(hexKey))
	if err != nil || len(raw) < 16 {
		return nil, fmt.Errorf("%w: invalid session key hex", ErrProvisioning)
	}
	return SessionKey(raw), nil
}

// refuseSessionKeyOnDisk fails if a same-UID-readable session.mac exists.
func refuseSessionKeyOnDisk(keyDir string) error {
	path := keyDir + string(os.PathSeparator) + SessionKeyFile
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: refusing same-UID-readable %s — use sealed session.rkey (R-owned) or FD",
			ErrProvisioning, SessionKeyFile)
	}
	return nil
}

// refuseInsecureSessionSources fails closed when secret was supplied via env/argv.
func refuseInsecureSessionSources() error {
	if os.Getenv("HERD_SIGNER_INSECURE_ENV_SESSION") == "1" {
		return fmt.Errorf("%w: HERD_SIGNER_INSECURE_ENV_SESSION forbids production acceptance", ErrUnsupportedPlatform)
	}
	if strings.TrimSpace(os.Getenv(EnvSessionKey)) != "" && os.Getenv("HERD_SIGNER_SESSION_KEY_FD") == "" && os.Getenv("HERD_SIGNER_SESSION_STDIN") != "1" {
		return fmt.Errorf("%w: %s in environment is same-UID readable; use sealed session.rkey or FD",
			ErrUnsupportedPlatform, EnvSessionKey)
	}
	return nil
}
