package provider

import "errors"

// OpFailureClass is the recovery/projection class for a provider operation
// error. Fleet lanes may map these to BLOCKED(provider_timeout) etc. without
// importing adapter internals. Empty string means no failure (err == nil).
//
// FAC-150 owns these classes; FAC-155 owns which configured provider is live.
type OpFailureClass string

const (
	// OpOK means err was nil.
	OpOK OpFailureClass = ""
	// OpTimeout is a bounded deadline or cancel (IsTimeout).
	OpTimeout OpFailureClass = "provider_timeout"
	// OpAmbiguous is a timed-out mutation whose write may or may not have landed.
	OpAmbiguous OpFailureClass = "provider_ambiguous"
	// OpProvider is a typed *ProviderError (HTTP/board rejection).
	OpProvider OpFailureClass = "provider_error"
	// OpOther is any other hard failure.
	OpOther OpFailureClass = "error"
)

// ClassifyOpError maps a provider operation error to a recovery class.
// Order: nil → timeout → ambiguous → ProviderError → other.
func ClassifyOpError(err error) OpFailureClass {
	if err == nil {
		return OpOK
	}
	if IsTimeout(err) {
		// Ambiguous mutations wrap a timeout write err; prefer ambiguous so
		// callers do not blind-retry as a plain timeout.
		if IsAmbiguous(err) {
			return OpAmbiguous
		}
		return OpTimeout
	}
	if IsAmbiguous(err) {
		return OpAmbiguous
	}
	var pe *ProviderError
	if errors.As(err, &pe) {
		return OpProvider
	}
	return OpOther
}

// IsRecoverableTimeout reports whether err is a pure timeout (not ambiguous
// mutation). Safe for capped read retries; unsafe for blind mutation retry.
func IsRecoverableTimeout(err error) bool {
	return ClassifyOpError(err) == OpTimeout
}
