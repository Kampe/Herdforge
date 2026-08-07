package signerboundary

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// LoadServeSessionKey loads the session MAC for the signer process without
// putting secret bytes in argv. Prefer FD, then stdin; reject --hex-style argv
// for production (caller must not pass secrets on the command line).
func LoadServeSessionKey(sessionKeyFD int, fromStdin bool, insecureHex string) (SessionKey, error) {
	if sessionKeyFD >= 0 {
		f := os.NewFile(uintptr(sessionKeyFD), "serve-session-fd")
		if f == nil {
			return nil, fmt.Errorf("invalid session key fd %d", sessionKeyFD)
		}
		return loadSessionKeyFromReader(f)
	}
	if fromStdin {
		return loadSessionKeyFromReader(os.Stdin)
	}
	if insecureHex != "" {
		if os.Getenv("HERD_SIGNER_INSECURE_ENV_SESSION") != "1" {
			return nil, fmt.Errorf("%w: --session-key-hex exposes secret in argv/ps; use --session-key-fd or --session-key-stdin (or HERD_SIGNER_INSECURE_ENV_SESSION=1 for non-acceptance tests only)",
				ErrUnsupportedPlatform)
		}
		return decodeSessionKeyHex(insecureHex)
	}
	// Also accept FD via env for launchd-style handoff (FD number only, not secret).
	if fdStr := strings.TrimSpace(os.Getenv("HERD_SIGNER_SESSION_KEY_FD")); fdStr != "" {
		fd, err := strconv.Atoi(fdStr)
		if err != nil {
			return nil, err
		}
		return LoadServeSessionKey(fd, false, "")
	}
	return nil, fmt.Errorf("%w: serve needs --session-key-fd N or --session-key-stdin", ErrProvisioning)
}
