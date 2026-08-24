package security

import (
	"fmt"
	"sync"
)

// tabBrokerRegistry owns broker lifetime keyed by tab ID. CloseTab / rollback
// must call CloseTabBroker so listeners cannot leak past the agent session.
type tabBrokerRegistry struct {
	mu    sync.Mutex
	byTab map[string]*HostAllowBroker
}

var globalTabBrokers = &tabBrokerRegistry{byTab: map[string]*HostAllowBroker{}}

// RegisterTabBroker binds a broker to a live tab. Replaces any prior broker.
func RegisterTabBroker(tabID string, b *HostAllowBroker) error {
	if tabID == "" || b == nil {
		return fmt.Errorf("register tab broker: tab and broker required")
	}
	globalTabBrokers.mu.Lock()
	defer globalTabBrokers.mu.Unlock()
	if old, ok := globalTabBrokers.byTab[tabID]; ok && old != nil && old != b {
		_ = old.Close()
	}
	globalTabBrokers.byTab[tabID] = b
	return nil
}

// CloseTabBroker closes and unregisters the broker for tabID (idempotent).
func CloseTabBroker(tabID string) error {
	if tabID == "" {
		return nil
	}
	globalTabBrokers.mu.Lock()
	b := globalTabBrokers.byTab[tabID]
	delete(globalTabBrokers.byTab, tabID)
	globalTabBrokers.mu.Unlock()
	if b == nil {
		return nil
	}
	return b.Close()
}

// BrokerForTab returns the broker for tabID if registered.
func BrokerForTab(tabID string) *HostAllowBroker {
	globalTabBrokers.mu.Lock()
	defer globalTabBrokers.mu.Unlock()
	return globalTabBrokers.byTab[tabID]
}
