package security

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrAgentNotFound is returned by LiveAgentResolver when the named agent is absent.
var ErrAgentNotFound = errors.New("security: agent not found")

// LiveAgentResolver looks up and retires live Herdr agents (production: herdr).
type LiveAgentResolver interface {
	Lookup(name string) (*LiveAgentIdentity, error)
	CloseTab(tabID string) error
}

// ContainedAgentResult is the agent name to prompt after ensure.
type ContainedAgentResult struct {
	AgentName      string
	Reused         bool
	Generation     string
	PolicyDigest   string
	TabID          string
	PaneID         string
	AgentSessionID string
}

// EnsureContainedAgent reuses a standing agent only when live Herdr identity
// (including agent_session_id) matches durable attestation. Otherwise it
// retires the stale tab with successful close + disappearance readback, then
// relaunches under LaunchAgent with the SAME standing name.
func EnsureContainedAgent(
	resolver LiveAgentResolver,
	sp ProcessSpawner,
	req AgentSpawnRequest,
	standingName string,
) (*ContainedAgentResult, *AgentSpawnResult, error) {
	if sp == nil {
		return nil, nil, fmt.Errorf("%w: nil spawner", ErrUnknownPolicy)
	}
	if req.Policy == nil || req.Grant == nil {
		return nil, nil, fmt.Errorf("%w: policy and grant required", ErrUnknownPolicy)
	}
	standingName = strings.TrimSpace(standingName)
	if standingName == "" {
		standingName = req.Name
	}
	req.Name = standingName
	if req.Label == "" {
		req.Label = standingName
	}

	if resolver != nil {
		live, lerr := resolver.Lookup(standingName)
		if lerr == nil && live != nil {
			att, aerr := RequireSessionAttestation(
				req.Policy.SharedCheckout, standingName, req.Policy, live,
				req.TaskRef, req.LeaseGeneration,
			)
			if aerr == nil {
				return &ContainedAgentResult{
					AgentName:      standingName,
					Reused:         true,
					Generation:     att.Generation,
					PolicyDigest:   att.PolicyDigest,
					TabID:          live.TabID,
					PaneID:         live.PaneID,
					AgentSessionID: live.AgentSessionID,
				}, nil, nil
			}
			if err := RetireStandingAgent(resolver, standingName, live.TabID); err != nil {
				_ = req.Policy.RecordFatal(EventPolicyBlock, "standing_retire_failed", err.Error())
				return nil, nil, fmt.Errorf("%w: %v", ErrSessionRetire, err)
			}
			_ = req.Policy.RecordFatal(EventPolicyBlock, "standing_tab_retired_stale", aerr.Error())
		} else if lerr != nil && !errors.Is(lerr, ErrAgentNotFound) {
			// Resolver outage/timeout — fail closed; do not treat as "gone".
			return nil, nil, fmt.Errorf("%w: live lookup: %v", ErrSessionRetire, lerr)
		}
	}

	spawn, err := LaunchAgent(sp, req)
	if err != nil {
		return nil, spawn, err
	}
	return &ContainedAgentResult{
		AgentName:      standingName,
		Reused:         false,
		Generation:     spawn.Generation,
		PolicyDigest:   spawn.PolicyDigest,
		TabID:          spawn.TabID,
		PaneID:         spawn.PaneID,
		AgentSessionID: spawn.AgentSessionID,
	}, spawn, nil
}

// RetireStandingAgent closes the tab and proves the standing name is gone.
// Lookup errors other than ErrAgentNotFound fail closed (not treated as gone).
func RetireStandingAgent(resolver LiveAgentResolver, name, tabID string) error {
	if resolver == nil {
		return fmt.Errorf("nil resolver")
	}
	if tabID == "" {
		return fmt.Errorf("empty tab id")
	}
	if err := resolver.CloseTab(tabID); err != nil {
		return fmt.Errorf("CloseTab: %w", err)
	}
	// Broker lifetime is owned by tab.
	if err := CloseTabBroker(tabID); err != nil {
		return fmt.Errorf("CloseTabBroker: %w", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		live, err := resolver.Lookup(name)
		if errors.Is(err, ErrAgentNotFound) || (err == nil && live == nil) {
			return nil // gone
		}
		if err != nil {
			// Outage/parse failure is NOT proof of disappearance.
			return fmt.Errorf("disappearance readback failed: %w", err)
		}
		if live.TabID != tabID {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("standing agent still present on tab %s after close", tabID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// StableStandingLease returns a durable lease key for standing agents so
// attestation reuse is possible across ensure calls (not random per launch).
func StableStandingLease(standingName string) string {
	return "standing:" + strings.TrimSpace(standingName)
}

// LeaseFromOpts formats the active claim/control lease generation for attestation.
// Fail-closed: lease must be >0 — never fabricates generation 1.
func LeaseFromOpts(lease int64) (string, error) {
	if lease <= 0 {
		return "", fmt.Errorf("%w: lease generation must be >0", ErrUnknownPolicy)
	}
	return fmt.Sprintf("%d", lease), nil
}

// MustLeaseFromOpts is for tests that already validated lease >0.
func MustLeaseFromOpts(lease int64) string {
	s, err := LeaseFromOpts(lease)
	if err != nil {
		panic(err)
	}
	return s
}
