package security

import (
	"net"
	"strings"
	"unicode"
)

// NormalizeHost validates and normalizes a worker-supplied host for allowlist match.
//
// Rejects: empty, userinfo, schemes, paths, spaces, CRLF, non-443/80 ports
// (port is stripped only when 443 or 80; other ports denied), raw IP except
// explicit loopback for tests when allowLoopback.
// Accepts: trailing-dot FQDN (dot stripped), IDNA/punycode via ToASCII.
func NormalizeHost(raw string, allowLoopback bool) (string, error) {
	h := strings.TrimSpace(raw)
	if h == "" {
		return "", &BlockedError{Reason: BlockBadHost, Code: "host_empty"}
	}
	if strings.ContainsAny(h, "\r\n\x00 \t") {
		return "", &BlockedError{Reason: BlockBadHost, Code: "host_control_chars"}
	}
	if strings.Contains(h, "://") || strings.Contains(h, "/") || strings.Contains(h, "@") {
		return "", &BlockedError{Reason: BlockBadHost, Code: "host_smuggling"}
	}
	// Bracketed IPv6 not supported for provider API hosts.
	if strings.HasPrefix(h, "[") {
		return "", &BlockedError{Reason: BlockBadHost, Code: "host_ipv6_unsupported"}
	}

	host := h
	if strings.Contains(h, ":") {
		// host:port — only default HTTPS/HTTP ports allowed, then stripped for exact match.
		hh, port, err := net.SplitHostPort(h)
		if err != nil {
			// Could be bare IPv6 without brackets — reject.
			return "", &BlockedError{Reason: BlockBadHost, Code: "host_port_invalid"}
		}
		if port != "443" && port != "80" {
			return "", &BlockedError{Reason: BlockBadHost, Code: "host_port_denied"}
		}
		host = hh
	}

	host = strings.TrimSuffix(host, ".")
	host = strings.ToLower(host)
	if host == "" {
		return "", &BlockedError{Reason: BlockBadHost, Code: "host_empty"}
	}

	// IP literals: only loopback when explicitly allowed (tests).
	if ip := net.ParseIP(host); ip != nil {
		if allowLoopback && ip.IsLoopback() {
			return "127.0.0.1", nil
		}
		return "", &BlockedError{Reason: BlockBadHost, Code: "host_ip_denied"}
	}

	// Production provider hosts are ASCII DNS labels. Non-ASCII / punycode input
	// must already be xn-- form; we reject raw non-ASCII (fail closed without
	// pulling an IDNA dependency into the security package).
	for _, r := range host {
		if r > unicode.MaxASCII || (!unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '.' && r != '-') {
			return "", &BlockedError{Reason: BlockBadHost, Code: "host_idna_invalid"}
		}
	}
	for _, part := range strings.Split(host, ".") {
		if part == "" || strings.HasPrefix(part, "-") || strings.HasSuffix(part, "-") {
			return "", &BlockedError{Reason: BlockBadHost, Code: "host_empty_label"}
		}
	}
	return host, nil
}

// ValidateAuthorizationMaterial rejects CRLF/header injection and dummy upstream use.
// Does not log or return the material.
func ValidateAuthorizationMaterial(auth string) error {
	if strings.TrimSpace(auth) == "" {
		return &BlockedError{Reason: BlockBadAuthMaterial, Code: "auth_empty"}
	}
	if strings.ContainsAny(auth, "\r\n\x00") {
		return &BlockedError{Reason: BlockBadAuthMaterial, Code: "auth_header_injection"}
	}
	// Single header value only — no smuggled second headers.
	if strings.Contains(auth, ":") && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth)), "bearer ") {
		// Allow "Bearer xxx" only as Authorization scheme form for API keys.
		// Some material is raw tokens without Bearer — those must not contain ':' either.
		if !strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			return &BlockedError{Reason: BlockBadAuthMaterial, Code: "auth_colon_denied"}
		}
	}
	// After "Bearer ", token must not contain whitespace or CRLF (already checked).
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(auth)), "bearer ") {
		tok := strings.TrimSpace(auth[len("Bearer "):])
		if tok == "" || strings.ContainsAny(tok, " \t") {
			return &BlockedError{Reason: BlockBadAuthMaterial, Code: "auth_bearer_invalid"}
		}
	}
	if IsDummyCredential(auth) {
		return &BlockedError{Reason: BlockDummyUpstream, Code: "auth_dummy"}
	}
	return nil
}

// normalizePathForMatch strips query and validates path safety.
func normalizePathForMatch(path string) (string, error) {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		return "", &BlockedError{Reason: BlockPathDenied, Code: "path_not_absolute"}
	}
	if strings.Contains(path, "..") || strings.Contains(path, "://") || strings.ContainsAny(path, "\r\n\x00") {
		return "", &BlockedError{Reason: BlockPathDenied, Code: "path_smuggling"}
	}
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	return path, nil
}
