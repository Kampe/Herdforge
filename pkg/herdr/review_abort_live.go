package herdr

import (
	"fmt"
	"strings"
)

// ReviewAbortLiveConflict refuses a coordinator abort when a live agent shares
// any of name, pane, or session with the bound launch but the remaining
// identity fields do not match. An exact idle/done unfocused match is allowed.
func ReviewAbortLiveConflict(agents []AgentEntry, reviewer, paneID, tabID, session string) error {
	reviewer = strings.TrimSpace(reviewer)
	paneID = strings.TrimSpace(paneID)
	tabID = strings.TrimSpace(tabID)
	session = strings.TrimSpace(session)
	for _, a := range agents {
		nameHit := strings.TrimSpace(a.Name) != "" && a.Name == reviewer
		paneHit := strings.TrimSpace(a.PaneID) != "" && a.PaneID == paneID
		sessHit := session != "" && strings.TrimSpace(a.Session.Value) != "" && a.Session.Value == session
		if !nameHit && !paneHit && !sessHit {
			continue
		}
		tabOK := tabID == "" || strings.TrimSpace(a.TabID) == "" || a.TabID == tabID
		exact := a.Name == reviewer && a.PaneID == paneID && tabOK
		if !exact {
			return fmt.Errorf("review abort refuses conflicting live identity: name=%s pane=%s session=%s", a.Name, a.PaneID, a.Session.Value)
		}
		if a.Session.Value != "" && a.Session.Value != session {
			return fmt.Errorf("live session mismatch: pane %s vs --session %s", a.Session.Value, session)
		}
		if a.Status == "working" {
			return fmt.Errorf("review abort refuses live useful work: reviewer %s is working", reviewer)
		}
		if a.Focused != nil && *a.Focused {
			return fmt.Errorf("review abort refuses a focused reviewer tab")
		}
	}
	return nil
}
