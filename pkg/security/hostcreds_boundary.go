package security

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Env vars for separate-UID HostCreds broker (FAC-170 / coordinated with FAC-169 style).
const (
	EnvBrokerUID = "HERD_HOSTCREDS_BROKER_UID" // required ≠ worker uid for production
	EnvBrokerPID = "HERD_HOSTCREDS_BROKER_PID" // live broker process for attach probe
	// EnvAllowSameUIDTest enables same-UID component tests ONLY. Production live
	// paths refuse this. Mutation tests assert live path fails when this is set
	// without a real separate-UID attestation.
	EnvAllowSameUIDTest = "HERD_HOSTCREDS_ALLOW_SAME_UID_TEST"
)

// BoundaryMechanism names the OS authority class.
const BoundaryMechanismSeparateUID = "separate-uid"

// HostCredsBoundary is an OS-enforced attestation that secrets live outside
// the worker UID. Same-UID process theater is never production-ready.
type HostCredsBoundary struct {
	Mechanism   string
	BrokerUID   int
	WorkerUID   int
	BrokerPID   int
	ProvedAt    time.Time
	ProbeDigest string
	// SecretPath if non-empty must be unreadable by worker UID.
	SecretPath string
}

// RequireProductionBoundary fails closed unless separate-UID is live-proved.
// Does not succeed on env scrub, FS sandbox, or same-UID "broker" process.
func RequireProductionBoundary() (*HostCredsBoundary, error) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, &BlockedError{Reason: BlockUnsupportedPlat, Code: "boundary_goos"}
	}
	workerUID := os.Getuid()
	raw := strings.TrimSpace(os.Getenv(EnvBrokerUID))
	if raw == "" {
		return nil, &BlockedError{Reason: BlockSecretExposure, Code: "broker_uid_unset"}
	}
	brokerUID, err := strconv.Atoi(raw)
	if err != nil || brokerUID < 0 {
		return nil, &BlockedError{Reason: BlockSecretExposure, Code: "broker_uid_invalid"}
	}
	if brokerUID == workerUID {
		return nil, &BlockedError{Reason: BlockSecretExposure, Code: "same_uid_refused"}
	}
	// Optional secret path ownership proof.
	secretPath := strings.TrimSpace(os.Getenv("HERD_HOSTCREDS_SECRET_PATH"))
	if secretPath != "" {
		if err := provePathUnreadableByWorker(secretPath); err != nil {
			return nil, err
		}
	}
	brokerPID := 0
	if p := strings.TrimSpace(os.Getenv(EnvBrokerPID)); p != "" {
		brokerPID, _ = strconv.Atoi(p)
	}
	if brokerPID > 0 {
		if err := proveAttachDenied(brokerPID, brokerUID); err != nil {
			return nil, err
		}
	} else {
		// Without PID we cannot prove attach denial — fail closed for production.
		return nil, &BlockedError{Reason: BlockSecretExposure, Code: "broker_pid_unset"}
	}

	digest := hashProbes([]string{
		"separate-uid",
		fmt.Sprintf("broker_uid=%d", brokerUID),
		fmt.Sprintf("worker_uid=%d", workerUID),
		fmt.Sprintf("broker_pid=%d", brokerPID),
		"key-unreadable",
		"attach-denied",
	})
	b := &HostCredsBoundary{
		Mechanism:   BoundaryMechanismSeparateUID,
		BrokerUID:   brokerUID,
		WorkerUID:   workerUID,
		BrokerPID:   brokerPID,
		ProvedAt:    time.Now().UTC(),
		ProbeDigest: digest,
		SecretPath:  secretPath,
	}
	return b, nil
}

// SameUIDTestAllowed reports unsafe test mode (never production).
func SameUIDTestAllowed() bool {
	return os.Getenv(EnvAllowSameUIDTest) == "1"
}

// provePathUnreadableByWorker requires ReadFile to fail for this process.
func provePathUnreadableByWorker(path string) error {
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		return &BlockedError{Reason: BlockSecretExposure, Code: "secret_readable_by_worker_uid"}
	}
	if err == nil {
		return &BlockedError{Reason: BlockSecretExposure, Code: "secret_empty_but_readable"}
	}
	// Permission errors expected.
	return nil
}

func hashProbes(parts []string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = h.Write([]byte(p))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
