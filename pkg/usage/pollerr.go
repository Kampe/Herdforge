package usage

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
)

// pollError carries a stable machine-readable code alongside the human detail.
// The code is the contract: consumers (herdr-quota, herdr-route) parse the
// prefix before the first ": " and never the prose after it. Every error a
// native poller can produce is one of these, so a quota failure is always
// classifiable without string-guessing. wrap preserves the underlying error
// chain (e.g. the net.Error for a timeout) for errors.As inspection.
type pollError struct {
	code string
	msg  string
	wrap error
}

func (e *pollError) Error() string { return e.code + ": " + e.msg }

func (e *pollError) Unwrap() error { return e.wrap }

func pollErrf(code, format string, args ...any) error {
	return &pollError{code: code, msg: fmt.Sprintf(format, args...)}
}

// httpStatusPollError names an unexpected HTTP status. 401/403 mean the
// credential on disk is no longer accepted; this collector never refreshes
// credentials, so the actionable fix is the owning CLI's login command.
func httpStatusPollError(surface string, status int) error {
	return &pollError{
		code: "http-" + strconv.Itoa(status),
		msg:  surface + ": HTTP " + strconv.Itoa(status),
	}
}

// RateLimitedPollError builds the same classified error a native poller
// returns on HTTP 429. Exported so a fixture poller registered from another
// package (e.g. a cmd/herd test exercising routeComputedQuota) can simulate a
// 429 without a real HTTP round trip; production pollers never call this.
func RateLimitedPollError(detail string) error {
	return pollErrf("rate-limited", "%s", detail)
}

func httpRateLimitPollError(surface string, resp *http.Response) error {
	retryAfter := strings.TrimSpace(resp.Header.Get("Retry-After"))
	if retryAfter == "" {
		retryAfter = "unspecified"
	}
	return &pollError{code: "rate-limited", msg: surface + ": HTTP 429; retry-after=" + retryAfter}
}

// netPollError classifies a transport failure so a hung provider is
// distinguishable from an unreachable one, preserving the original error for
// errors.As inspection.
func netPollError(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return &pollError{code: "timeout", msg: err.Error(), wrap: err}
	}
	return &pollError{code: "unreachable", msg: err.Error(), wrap: err}
}

// classifyPollError renders any error as "<code>: <detail>". Errors produced
// inside this package are already pollErrors and pass through unchanged;
// anything unexpected is labelled unknown rather than guessed at.
func classifyPollError(err error) string {
	if err == nil {
		return ""
	}
	var pe *pollError
	if errors.As(err, &pe) {
		return pe.Error()
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "timeout: " + err.Error()
	}
	if errors.As(err, &ne) {
		return "unreachable: " + err.Error()
	}
	return "unknown: " + err.Error()
}

// pollErrorCode extracts just the stable code.
func pollErrorCode(err error) string {
	s := classifyPollError(err)
	for i := 0; i < len(s); i++ {
		if s[i] == ':' {
			return s[:i]
		}
	}
	return s
}

// pollErrorCodeFromDetail extracts the stable code from an ALREADY-CLASSIFIED
// detail string, as carried in UsageSnapshot.Errors. The snapshot stores the
// rendered "<code>: <detail>" form rather than the error value, so consumers
// downstream of a fetch have only the string to classify from.
func pollErrorCodeFromDetail(detail string) string {
	for i := 0; i < len(detail); i++ {
		if detail[i] == ':' {
			return detail[:i]
		}
	}
	return detail
}
