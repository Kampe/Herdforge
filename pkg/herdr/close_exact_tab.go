package herdr

import (
	"errors"
	"fmt"
	"strings"
)

// FAC-569: closing one exact tab we own was implemented THREE times.
//
//   - defaultCleanupClose: fenced attempt, then delegate to a plain close on ANY
//     blocked outcome, then prove absence.
//   - hardCloseTab: fenced attempt, refuse on conflict, degrade only on a
//     capability gap, then prove absence.
//   - CloseOwnTab: reuses the cleanup contract for a live agent, plain close
//     when no agent row exists.
//
// The copies disagreed on the one question that matters. defaultCleanupClose
// delegated even on a stale generation, which is a real conflict and must keep
// refusing; hardCloseTab refused even when the installed herdr has no
// compare-close support at all, which stranded the tab it had just created. Only
// one rule can be right, so there is now only one.

// CapabilityGapReason reports whether a refusal means this herdr build CANNOT
// fence a close, as opposed to refusing this particular close.
//
// The distinction is the whole contract. A capability gap must degrade to a
// plain close plus an absence readback, or a failed launch orphans its own tab
// and an operator closes it by hand. A genuine conflict — stale generation,
// changed attachment, active mutation, protected — must keep refusing, or a
// close race can recycle-kill a tab that gained a new agent.
func CapabilityGapReason(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, conflict := range []string{
		"stale-generation", "attachment-changed", "active-mutation", "protected",
		"unresolved intent", "without resulting absence",
	} {
		if strings.Contains(msg, conflict) {
			return false
		}
	}
	for _, gap := range []string{
		"unknown command", "unrecognized command", "unknown subcommand",
		"invalid choice", "not a herdr command", "no such command",
		"unknown flag", "command not found", "usage:",
		// FAC-576: the installed herdr answers an unknown `tab` subcommand by
		// printing its subcommand banner, which says "herdr tab commands:" and
		// never the word "usage". Matching only "usage:" meant the gap went
		// undetected on the exact build that has the gap, so a failed launch
		// still stranded its tab. Observed verbatim on this host.
		"tab commands:", "commands:",
		"no immutable generation", "generation evidence is unavailable",
		// "generation is required" means we could not SUPPLY generation
		// evidence, so this build/row cannot be fenced at all. That is a gap.
		// It is not "stale-generation", which means we did supply one and it
		// was wrong — a real conflict that must keep refusing.
		"generation is required",
	} {
		if strings.Contains(msg, gap) {
			return true
		}
	}
	return false
}

// ExactTabIdentity is everything needed to close one tab and prove it is gone.
type ExactTabIdentity struct {
	Workspace  string
	TabID      string
	Name       string
	PaneID     string
	SessionID  string
	Kind       string
	Revision   uint64
	Generation string // empty when the build exposes none
	Nonce      string
}

// CloseExactTabOutcome is the result of one close attempt.
type CloseExactTabOutcome struct {
	Closed    bool
	Delegated bool
	Reason    string
}

// CloseExactTab is THE definition of closing one exact tab this process owns.
//
// It prefers the fenced compare-and-close. On a capability gap it degrades
// loudly to a plain close of that exact tab. On a real conflict it refuses. In
// every successful case absence is proven by readback, because a close request
// is not evidence that Herdr released the tab.
func CloseExactTab(id ExactTabIdentity) (CloseExactTabOutcome, error) {
	if strings.TrimSpace(id.TabID) == "" {
		return CloseExactTabOutcome{}, fmt.Errorf("close exact tab: tab id is required")
	}
	nonce := id.Nonce
	if strings.TrimSpace(nonce) == "" {
		nonce = fmt.Sprintf("close-exact-%s-%d", id.TabID, id.Revision)
	}
	req := CloseRequest{
		WorkspaceID: id.Workspace,
		TabID:       id.TabID,
		Generation:  id.Generation,
		TabRevision: id.Revision,
		Nonce:       nonce,
	}
	if id.PaneID != "" {
		req.PaneIDs = []string{id.PaneID}
	}
	if id.SessionID != "" {
		req.SessionID = id.SessionID
	}
	if id.Kind != "" {
		req.Agent = id.Kind
	}

	delegated := false
	if err := TabCloseCAS(req); err != nil {
		var blocked *CloseUnavailableError
		isBlocked := errors.As(err, &blocked)
		gap := CapabilityGapReason(err) || (isBlocked && strings.TrimSpace(id.Generation) == "" && CapabilityGapReason(errors.New(blocked.Reason)))
		if !gap {
			// A real refusal. Never blind-close what could not be fenced.
			return CloseExactTabOutcome{Reason: err.Error()}, err
		}
		// Deliberately no write to stderr here: this is a library on the path
		// of machine-readable CLI output, and a stray line corrupts it. The
		// degradation is reported instead through Delegated and Reason, which
		// callers surface — so it is observable without being noise.
		if closeErr := tabCloseRaw(id.TabID); closeErr != nil {
			return CloseExactTabOutcome{Reason: "delegation failed"}, errors.Join(err, closeErr)
		}
		delegated = true
	}

	// Absence readback is the invariant that survives either path.
	live, rbErr := AgentList()
	if rbErr != nil {
		return CloseExactTabOutcome{Delegated: delegated, Reason: "absence readback failed"},
			fmt.Errorf("close exact tab %s: absence readback: %w", id.TabID, rbErr)
	}
	for _, a := range live {
		if a.TabID != id.TabID {
			continue
		}
		// When a name was supplied, require the FULL identity to match before
		// calling it still-present: a recycled tab id with a different agent is
		// not the thing we tried to close.
		if id.Name != "" && id.PaneID != "" && (a.Name != id.Name || a.PaneID != id.PaneID) {
			continue
		}
		return stillPresent(id, delegated)
	}
	// The agent list only sees tabs that HAVE an agent. A leaked tab with no
	// agent attached is precisely the orphan case this exists to prevent, so
	// when the workspace is known the tab list must confirm absence too.
	if ws := strings.TrimSpace(id.Workspace); ws != "" {
		tabs, tlErr := TabList(ws)
		if tlErr != nil {
			return CloseExactTabOutcome{Delegated: delegated, Reason: "tab absence readback failed"},
				fmt.Errorf("close exact tab %s: tab absence readback: %w", id.TabID, tlErr)
		}
		for _, tab := range tabs {
			if tab.TabID == id.TabID {
				return stillPresent(id, delegated)
			}
		}
	}
	reason := "fenced compare-and-close; absence confirmed"
	if delegated {
		reason = "delegated exact-tab close via Herdr; absence confirmed"
	}
	return CloseExactTabOutcome{Closed: true, Delegated: delegated, Reason: reason}, nil
}

// stillPresent reports a close that did not achieve absence.
//
// A DELEGATED close that leaves the tab present is a refusal we understand: we
// could not fence it and the unfenced close did not take, so the caller should
// treat it as blocked and leave the tab alone. A FENCED close that leaves the
// tab present is different — the server said it closed and it did not, which is
// an inconsistency, not a policy refusal. Collapsing the two would let a real
// inconsistency be reported as an ordinary block.
func stillPresent(id ExactTabIdentity, delegated bool) (CloseExactTabOutcome, error) {
	out := CloseExactTabOutcome{Delegated: delegated, Reason: "tab still present after close"}
	if delegated {
		return out, &CloseUnavailableError{
			TabID:  id.TabID,
			Reason: "delegated absence readback: exact tab identity still present",
		}
	}
	return out, fmt.Errorf("close exact tab %s: absence readback: still present after close", id.TabID)
}
