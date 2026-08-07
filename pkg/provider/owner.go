package provider

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"sync"
)

var (
	ownerOnce    sync.Once
	processOwner string
	ownerInitErr error
)

// ProcessOwnerID returns a cryptographic process identity for lease
// ownership. Format: herd1.<host>.<pid>.<32-byte-hex-nonce>.
// Ambient HERD_OWNER_ID is never trusted. crypto/rand failure propagates
// — never falls back to low-entropy host+PID hashes.
func ProcessOwnerID() (string, error) {
	ownerOnce.Do(func() {
		host, err := os.Hostname()
		if err != nil || host == "" {
			host = "herd"
		}
		var nonce [32]byte
		if _, err := rand.Read(nonce[:]); err != nil {
			ownerInitErr = fmt.Errorf("provider: crypto/rand failed for process owner identity: %w", err)
			return
		}
		processOwner = fmt.Sprintf("herd1.%s.%d.%s", host, os.Getpid(), hex.EncodeToString(nonce[:]))
	})
	if ownerInitErr != nil {
		return "", ownerInitErr
	}
	if processOwner == "" {
		return "", fmt.Errorf("provider: process owner identity unset")
	}
	return processOwner, nil
}

// DefaultOwnerID returns ProcessOwnerID or a fail-closed sentinel that
// cannot acquire leases (empty string is rejected by Claim). Callers that
// must surface the error should use ProcessOwnerID.
//
// Deprecated for new call sites: prefer ProcessOwnerID and propagate err.
func DefaultOwnerID() string {
	id, err := ProcessOwnerID()
	if err != nil {
		// Empty owner cannot satisfy claim fencing; fail closed at Claim.
		return ""
	}
	return id
}

// HerdrSessionOwnerID binds lease handoff to a live Herdr agent session.
// Inputs are tab/pane/agent identity plus the proven prompt receipt token
// so a coordinator cannot invent a fake worker owner without the launch
// proof material.
func HerdrSessionOwnerID(tabID, paneID, agentName, receiptToken string) string {
	sum := sha256.Sum256([]byte(stringsJoinSession(tabID, paneID, agentName, receiptToken)))
	return "herdr1." + hex.EncodeToString(sum[:])
}

func stringsJoinSession(parts ...string) string {
	out := "herdr-session"
	for _, p := range parts {
		out += "\x1f" + p
	}
	return out
}
