package mail

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

// waiterTokenBody is the durable identity of a queue waiter. PID alone is
// insufficient (reuse after exit/reboot); StartNS + BootID bind the token
// to a specific process incarnation.
type waiterTokenBody struct {
	PID     int    `json:"pid"`
	StartNS int64  `json:"start_ns"`
	BootID  string `json:"boot_id,omitempty"`
}

var (
	selfIdentOnce sync.Once
	selfIdent     waiterTokenBody
	selfIdentErr  error
)

// selfTokenIdentity captures this process's liveness fingerprint once.
func selfTokenIdentity() (waiterTokenBody, error) {
	selfIdentOnce.Do(func() {
		pid := os.Getpid()
		start, err := processStartNS(pid)
		if err != nil {
			selfIdentErr = err
			return
		}
		selfIdent = waiterTokenBody{
			PID:     pid,
			StartNS: start,
			BootID:  bootIdentity(),
		}
	})
	return selfIdent, selfIdentErr
}

// encodeTokenBody serializes a waiter token (no host paths).
func encodeTokenBody(t waiterTokenBody) ([]byte, error) {
	return json.Marshal(t)
}

// parseTokenBody parses a token file. Legacy PID-only tokens are treated as
// ambiguous (cannot safely reap) by returning ok=false with err=nil and
// body.PID set when possible — callers must not reap ambiguous tokens.
func parseTokenBody(data []byte) (body waiterTokenBody, complete bool, err error) {
	s := string(data)
	// Prefer JSON.
	if json.Unmarshal(data, &body) == nil && body.PID > 0 && body.StartNS > 0 {
		return body, true, nil
	}
	// Legacy "pid\n" only — incomplete identity.
	var pid int
	if _, scanErr := fmt.Sscanf(s, "%d", &pid); scanErr == nil && pid > 0 {
		return waiterTokenBody{PID: pid}, false, nil
	}
	return waiterTokenBody{}, false, fmt.Errorf("unreadable waiter token")
}

// tokenLiveness classifies a token:
//   - live: process matches pid+start(+boot when available)
//   - dead: confident the waiter is gone (safe to reap)
//   - ambiguous: cannot decide (must not reap; stuckGrace prevents forever wait)
type tokenLiveness int

const (
	tokenLive tokenLiveness = iota
	tokenDead
	tokenAmbiguous
)

func classifyToken(body waiterTokenBody, complete bool) tokenLiveness {
	if body.PID <= 0 {
		return tokenDead
	}
	if !processAlive(body.PID) {
		return tokenDead
	}
	if !complete || body.StartNS <= 0 {
		// PID responds but we lack start identity — do not reap (may be live).
		return tokenAmbiguous
	}
	cur, err := processStartNS(body.PID)
	if err != nil {
		// Process appears alive (kill succeeded) but start unreadable.
		return tokenAmbiguous
	}
	if cur != body.StartNS {
		// PID reuse: different process incarnation.
		return tokenDead
	}
	if body.BootID != "" {
		boot := bootIdentity()
		if boot != "" && boot != body.BootID {
			// Boot identity changed (reboot) while file persisted — dead.
			return tokenDead
		}
	}
	return tokenLive
}
