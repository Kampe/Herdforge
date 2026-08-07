// Package toolprobe owns artifact-backed tool-execution probes and their
// versioned, TTL-cached receipts (FAC-139).
//
// A generation surface that merely describes a write is not write-capable.
// Only a current PASS receipt for the exact provider/model/harness/recipe/
// toolchain identity may admit a write-capable launch.
package toolprobe

// Status is the classified outcome of one artifact tool-probe.
// Reviewers and routers treat any non-PASS as fail-closed for write-capable
// launch; classification drives deterministic failover and operator diagnosis.
type Status string

const (
	StatusPASS      Status = "PASS"
	StatusINCAPABLE Status = "INCAPABLE"
	StatusUNKNOWN   Status = "UNKNOWN"
	StatusAUTH      Status = "AUTH"
	StatusQUOTA     Status = "QUOTA"
	StatusRateLimit Status = "RATE_LIMIT"
	StatusTOOLING   Status = "TOOLING"
)

// WriteCapable is true only for an explicit PASS.
func (s Status) WriteCapable() bool { return s == StatusPASS }

// Retryable reports whether a classified failure may clear after time or a
// different attempt (quota/rate/auth/tooling). INCAPABLE is sticky for the TTL.
func (s Status) Retryable() bool {
	switch s {
	case StatusQUOTA, StatusRateLimit, StatusAUTH, StatusTOOLING, StatusUNKNOWN:
		return true
	default:
		return false
	}
}

// Valid reports whether s is one of the defined statuses.
func (s Status) Valid() bool {
	switch s {
	case StatusPASS, StatusINCAPABLE, StatusUNKNOWN, StatusAUTH, StatusQUOTA, StatusRateLimit, StatusTOOLING:
		return true
	default:
		return false
	}
}
