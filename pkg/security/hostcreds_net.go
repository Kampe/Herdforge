package security

import (
	"fmt"
	"net"
	"strings"
)

// resolveAndPinIP resolves host and rejects DNS rebinding to private/link-local
// addresses (except explicit loopback when allowLoopback on ParseIP path).
func resolveAndPinIP(host string) (net.IP, error) {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return nil, fmt.Errorf("empty host")
	}
	if ip := net.ParseIP(host); ip != nil {
		if err := validateDialIP(ip, true); err != nil {
			return nil, err
		}
		return ip, nil
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return nil, fmt.Errorf("dns: %w", err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("dns: no addresses")
	}
	var chosen net.IP
	for _, ip := range ips {
		// Production: never dial private targets via rebinding.
		if err := validateDialIP(ip, false); err != nil {
			return nil, fmt.Errorf("dns rebind denied for %s: %w", host, err)
		}
		if chosen == nil || (ip.To4() != nil && chosen.To4() == nil) {
			chosen = ip
		}
	}
	if chosen == nil {
		return nil, fmt.Errorf("dns: no usable address for %s", host)
	}
	return chosen, nil
}

func validateDialIP(ip net.IP, allowLoopback bool) error {
	if ip == nil {
		return fmt.Errorf("nil ip")
	}
	if ip.IsLoopback() {
		if allowLoopback {
			return nil
		}
		return fmt.Errorf("loopback denied")
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
		return fmt.Errorf("private/link-local destination denied")
	}
	return nil
}
