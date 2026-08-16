package main

import (
	"fmt"
	"os"
	"strings"
)

// productionMode is deliberately opt-in. A developer checkout is a local
// Herdr client by default: routing and pane lifecycle still go through Herdr,
// but hosted signer/attestation controls are not required. Set HERD_MODE=production
// when this binary is operating a hosted or shared control plane.
func productionMode() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("HERD_MODE"))) {
	case "production", "prod", "hosted":
		return true
	case "local", "dev", "development":
		return false
	default:
		// A control secret is an explicit signal that the caller opted into the
		// hosted MAC control plane. Plain local Herdr use remains zero-config.
		return strings.TrimSpace(os.Getenv("HERD_CONTROL_SECRET")) != ""
	}
}

func modeLabel() string {
	if productionMode() {
		return "production"
	}
	return "local"
}

func requireProductionMode(feature string) error {
	if !productionMode() {
		return fmt.Errorf("%s requires HERD_MODE=production", feature)
	}
	return nil
}
