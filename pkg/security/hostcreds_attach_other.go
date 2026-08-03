//go:build !darwin && !linux

package security

func proveAttachDenied(brokerPID, brokerUID int) error {
	_ = brokerPID
	_ = brokerUID
	return &BlockedError{Reason: BlockUnsupportedPlat, Code: "attach_probe_unsupported"}
}
