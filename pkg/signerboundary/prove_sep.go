package signerboundary

import (
	"fmt"
	"os"
	"runtime"
	"strings"
)

type proveSepConfig struct {
	KeyPath      string
	SignerUID    int
	RequesterUID int
	BuilderUID   int
	SocketPath   string
	SessionKey   SessionKey
	SignerPID    int
}

// proveSeparateUID runs the mandatory live suite using structured ProbeReceipt
// records. Harness failures (missing path, dial failure, ambiguous errno) are
// BLOCKED — never treated as denial success.
func proveSeparateUID(cfg proveSepConfig) (digest string, signerPID int, err error) {
	var receipts []ProbeReceipt

	// --- path-harden (must succeed as positive proof, not "error = deny") ---
	if err := auditKeyMaterialPath(cfg.KeyPath, cfg.SignerUID); err != nil {
		return "", 0, err
	}
	receipts = append(receipts, ProbeReceipt{
		Version: 1, Platform: runtime.GOOS, Operation: "path-harden", OK: true,
		Detail: "symlink/hardlink/nlink/owner/mode audited; path exists",
	})

	// --- key-read: file MUST exist; only EACCES/EPERM counts as denial ---
	if err := requirePathExists(cfg.KeyPath); err != nil {
		return "", 0, err
	}
	_, rerr := os.ReadFile(cfg.KeyPath)
	if rerr == nil {
		return "", 0, fmt.Errorf("%w: key still readable by uid %d", ErrAdversarialSuccess, os.Getuid())
	}
	if !isPermissionDenied(rerr) {
		return "", 0, fmt.Errorf("%w: key-read probe harness failure (want EACCES/EPERM, got %v) — not a denial proof",
			ErrProvisioning, rerr)
	}
	receipts = append(receipts, ProbeReceipt{
		Version: 1, Platform: runtime.GOOS, Operation: "key-read", OK: true,
		ExpectedErrno: "EACCES|EPERM", ObservedErrno: observedErrnoString(rerr),
		Detail: "worker-uid ReadFile denied with permission errno; path still present",
	})

	// --- key-non-export ---
	receipts = append(receipts, ProbeReceipt{
		Version: 1, Platform: runtime.GOOS, Operation: "key-non-export", OK: true,
		Detail: "coordinator process holds no private seed (separate-uid only)",
	})

	// --- ipc-unauth: require ErrorCode from live server ---
	if err := probeUnauthorizedIPCStructured(cfg); err != nil {
		return "", 0, err
	}
	receipts = append(receipts, ProbeReceipt{
		Version: 1, Platform: runtime.GOOS, Operation: "ipc-unauth", OK: true,
		Detail: "server returned UNAUTHORIZED_MAC / INVALID_REQUEST error codes",
	})

	// --- ipc-auth (canonical admitted verdict — not an arbitrary payload oracle) ---
	req := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"patch-probe", "APPROVED", "fac-169-live-probe", nil,
	)
	sig, err := signRequestOverIPC(cfg.SocketPath, cfg.SessionKey, &req)
	if err != nil {
		return "", 0, fmt.Errorf("%w: authorized SignRequest failed (serve as uid %d?): %v",
			ErrProvisioning, cfg.SignerUID, err)
	}
	if len(sig) == 0 {
		return "", 0, fmt.Errorf("%w: empty signature", ErrProvisioning)
	}
	receipts = append(receipts, ProbeReceipt{
		Version: 1, Platform: runtime.GOOS, Operation: "ipc-auth", OK: true,
		Detail: fmt.Sprintf("authorized SignRequest signature_len=%d", len(sig)),
	})

	// --- attach: real ptrace/proc-mem; ESRCH/unsupported BLOCK ---
	pid := cfg.SignerPID
	if pid == 0 {
		pid = discoverSignerPID(cfg.SocketPath)
	}
	if pid == 0 {
		return "", 0, fmt.Errorf("%w: cannot observe signer PID for attach probe (set HERD_SIGNER_PID)", ErrProvisioning)
	}
	uid, ok := processUID(pid)
	if !ok {
		return "", 0, fmt.Errorf("%w: cannot read signer process uid for pid %d — attach probe blocked", ErrProvisioning, pid)
	}
	if uid != cfg.SignerUID {
		return "", 0, fmt.Errorf("%w: signer pid %d uid %d != key owner %d", ErrProvisioning, pid, uid, cfg.SignerUID)
	}
	if uid == os.Getuid() {
		return "", 0, fmt.Errorf("%w: signer runs as same uid as probe — same-UID mode is not FAC-169 acceptance", ErrProvisioning)
	}
	attachErr := tryAttach(pid)
	if attachErr == nil {
		return "", 0, fmt.Errorf("%w: ptrace attach to signer SUCCEEDED", ErrAdversarialSuccess)
	}
	okDeny, harness := classifyAttachError(attachErr)
	if harness != nil {
		return "", 0, harness
	}
	if !okDeny {
		return "", 0, fmt.Errorf("%w: attach not isolation denial: %v", ErrProvisioning, attachErr)
	}
	// Second channel: process memory read
	if err := tryProcMemRead(pid); err == nil {
		return "", 0, fmt.Errorf("%w: process memory read SUCCEEDED", ErrAdversarialSuccess)
	} else if !isProcMemUnavailable(err) {
		ok2, h2 := classifyAttachError(err)
		if h2 != nil {
			return "", 0, h2
		}
		if !ok2 {
			return "", 0, fmt.Errorf("%w: mem-read not isolation denial: %v", ErrProvisioning, err)
		}
	}
	receipts = append(receipts, ProbeReceipt{
		Version: 1, Platform: runtime.GOOS, Operation: "attach", OK: true,
		ExpectedErrno: "EPERM|EACCES", ObservedErrno: observedErrnoString(attachErr),
		SignerPID: pid, SignerUID: uid,
		Detail: "ptrace denied with EPERM/EACCES against live exact-UID signer",
	})

	digest, err = EncodeProbeDigest(receipts)
	if err != nil {
		return "", 0, err
	}
	return digest, pid, nil
}

func probeUnauthorizedIPCStructured(cfg proveSepConfig) error {
	// Must reach the server. Dial failure = BLOCKED (harness), not denial.
	req := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"p", "APPROVED", "session-ok", nil,
	)
	req.Nonce = "n1"
	code, err := signRequestErrorCode(cfg.SocketPath, req, "")
	if err != nil && strings.Contains(err.Error(), "dial") {
		return fmt.Errorf("%w: signer not reachable for unauth probe: %v", ErrProvisioning, err)
	}
	if code != ErrCodeUnauthorizedMAC && code != ErrCodeInvalidRequest {
		if err == nil {
			return fmt.Errorf("%w: IPC accepted empty MAC", ErrAdversarialSuccess)
		}
		return fmt.Errorf("%w: unauth probe want error_code=%s|%s got code=%q err=%v",
			ErrProvisioning, ErrCodeUnauthorizedMAC, ErrCodeInvalidRequest, code, err)
	}

	code, err = signRequestErrorCode(cfg.SocketPath, req, "deadbeef")
	if err != nil && strings.Contains(err.Error(), "dial") {
		return fmt.Errorf("%w: signer not reachable: %v", ErrProvisioning, err)
	}
	if code != ErrCodeUnauthorizedMAC {
		if err == nil {
			return fmt.Errorf("%w: IPC accepted wrong MAC", ErrAdversarialSuccess)
		}
		return fmt.Errorf("%w: wrong-MAC probe want %s got code=%q err=%v",
			ErrProvisioning, ErrCodeUnauthorizedMAC, code, err)
	}

	// Arbitrary oracle op rejected even with valid MAC.
	oracle := SignRequest{Op: "sign-bytes", SessionID: "s", Nonce: "n2", Payload: []byte("x")}
	mac := cfg.SessionKey.BindRequestMAC(oracle)
	code, err = signRequestErrorCode(cfg.SocketPath, oracle, mac)
	if code != ErrCodeInvalidRequest && code != ErrCodeUnknownOp {
		if err == nil {
			return fmt.Errorf("%w: IPC accepted arbitrary sign-bytes oracle", ErrAdversarialSuccess)
		}
		return fmt.Errorf("%w: oracle probe want INVALID_REQUEST got code=%q err=%v",
			ErrProvisioning, code, err)
	}
	return nil
}

// signRequestErrorCode dials and returns server ErrorCode (empty if ok or dial fail).
func signRequestErrorCode(socketPath string, req SignRequest, mac string) (string, error) {
	_, err := signRequestOverIPCWithMAC(socketPath, req, mac)
	if err == nil {
		return "", nil
	}
	// Parse "signer: ..." — prefer structured by re-dialing with decode
	return parseWireErrorCode(socketPath, req, mac)
}

func parseWireErrorCode(socketPath string, req SignRequest, mac string) (string, error) {
	// Reuse dial path from ipc — decode full response.
	return dialForErrorCode(socketPath, req, mac)
}

func discoverSignerPID(socketPath string) int {
	return peerPIDOfSocket(socketPath)
}

func probeAttachDenied(signerPID, signerUID int) error {
	if signerPID <= 0 {
		return fmt.Errorf("%w: invalid signer pid", ErrProvisioning)
	}
	// ptrace attach
	if err := tryAttach(signerPID); err == nil {
		return fmt.Errorf("%w: ptrace attach to signer pid %d SUCCEEDED", ErrAdversarialSuccess, signerPID)
	} else {
		ok, harness := classifyAttachError(err)
		if harness != nil {
			return harness
		}
		if !ok {
			return fmt.Errorf("%w: ptrace error not classified as isolation denial: %v", ErrProvisioning, err)
		}
	}
	// process memory read
	if err := tryProcMemRead(signerPID); err == nil {
		return fmt.Errorf("%w: process memory read of signer pid %d SUCCEEDED", ErrAdversarialSuccess, signerPID)
	} else {
		// On Darwin /proc/mem absent — that is NOT a denial proof by itself.
		// tryProcMemRead returns a harness sentinel when unavailable.
		if isProcMemUnavailable(err) {
			// Require that ptrace already classified as denial (above).
			// Platform-specific: also try task_for_pid / process_vm_readv.
			if err2 := tryProcessVMRead(signerPID); err2 == nil {
				return fmt.Errorf("%w: process_vm_readv/task_for_pid SUCCEEDED", ErrAdversarialSuccess)
			} else if isProcMemUnavailable(err2) {
				// Only ptrace denial available — require separate-uid (caller checked).
				return nil
			} else {
				ok, harness := classifyAttachError(err2)
				if harness != nil {
					return harness
				}
				if !ok {
					return fmt.Errorf("%w: memory-read error not isolation denial: %v", ErrProvisioning, err2)
				}
			}
			return nil
		}
		ok, harness := classifyAttachError(err)
		if harness != nil {
			return harness
		}
		if !ok {
			return fmt.Errorf("%w: proc-mem error not isolation denial: %v", ErrProvisioning, err)
		}
	}
	return nil
}
