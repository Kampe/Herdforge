package herdr

import (
	"errors"
	"fmt"
	"strings"
)

// CloseOwnTab compensates a tab this process just created, when the launch it
// was created for failed.
//
// FAC-550: the failure paths called TabClose, which is an unconditional
// CloseUnavailableError stub, so every failed dispatch printed
//
//	tab close BLOCKED close unavailable: FAC-180 atomic generation/session
//	compare-and-close is required; use TabCloseCAS
//
// and leaked the tab. Five consecutive failures leaked five tabs. The FAC-180
// refusal is correct -- a caller must not blind-close a tab it cannot fence --
// but the compensating caller was never migrated to the fenced API.
//
// The fenced path is always attempted first. It needs the live agent row for
// workspace/revision/session evidence; when the launch died before the agent
// registered, that row does not exist and no generation can be produced. In
// exactly that case this delegates the EXACT tab id to Herdr's own close, then
// verifies by readback that the tab is gone. That is the same delegation
// defaultCleanupClose already relies on, held to the same absence proof.
//
// This is deliberately narrow: it closes one exact tab id the caller created.
// It is not a sweep and must never be used to reap tabs owned by anyone else.
func CloseOwnTab(tabID string) error {
	tabID = strings.TrimSpace(tabID)
	if tabID == "" {
		return fmt.Errorf("herdr close own tab: tab id is required")
	}

	agents, listErr := AgentList()
	if listErr == nil {
		for _, agent := range agents {
			if agent.TabID != tabID {
				continue
			}
			// A live agent row carries the fencing evidence; use the same
			// fenced attempt-then-delegate contract as fleet cleanup.
			attempt := currentCleanupClose()(agent)
			switch attempt.Outcome {
			case CleanupClosed:
				return nil
			case CleanupBlocked:
				return &CloseUnavailableError{TabID: tabID, Reason: attempt.Reason}
			default:
				return fmt.Errorf("herdr close own tab %s: %s", tabID, attempt.Reason)
			}
		}
	}

	// No agent row: the launch failed before the agent registered, so there is
	// nothing to fence against. Close the exact tab and prove it is gone.
	workspace := workspaceForTab(tabID, agents)
	if err := tabCloseRaw(tabID); err != nil {
		return fmt.Errorf("herdr close own tab %s: %w", tabID, err)
	}
	if workspace == "" {
		// Without a workspace the absence readback is not available. The close
		// itself succeeded; report that rather than inventing a proof.
		return nil
	}
	tabs, err := TabList(workspace)
	if err != nil {
		return fmt.Errorf("herdr close own tab %s: absence readback: %w", tabID, err)
	}
	for _, tab := range tabs {
		if tab.TabID == tabID {
			return fmt.Errorf("herdr close own tab %s: tab still present after close", tabID)
		}
	}
	return nil
}

// workspaceForTab resolves the workspace owning a tab from any live agent that
// shares it, falling back to the tab id's own workspace prefix (Herdr ids are
// formatted "<workspace>:<id>").
func workspaceForTab(tabID string, agents []AgentEntry) string {
	for _, agent := range agents {
		if agent.TabID == tabID && strings.TrimSpace(agent.Workspace) != "" {
			return agent.Workspace
		}
	}
	if idx := strings.Index(tabID, ":"); idx > 0 {
		return tabID[:idx]
	}
	return ""
}

// IsCloseUnavailable reports whether an error is the FAC-180 fencing refusal.
func IsCloseUnavailable(err error) bool {
	var blocked *CloseUnavailableError
	return errors.As(err, &blocked)
}
