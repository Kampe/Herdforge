// Package transcript is the read-only diagnostic that lets a coordinator read
// a lane's recent output and final handoff through Herdforge.
//
// FAC-551: Herdforge had no such command -- herd transcript, pane-read, read,
// tail, logs and handoff all did not exist -- and its own top-level help ended
// with "agent/pane operations  Use the herdr binary". Every coordinator that
// needed a finished lane's report therefore had to leave the Herdforge
// boundary. That is a missing diagnostic, not operator shortcutting.
//
// Everything here is strictly read-only. It never submits text, presses keys,
// or touches tab lifecycle.
package transcript

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/herdr"
)

// Result is one lane's readable state. Text is the recent pane output;
// Handoff is the lane's final reported block when one can be isolated.
type Result struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"`
	Status    string `json:"status,omitempty"`
	PaneID    string `json:"pane_id,omitempty"`
	TabID     string `json:"tab_id,omitempty"`
	Workspace string `json:"workspace_id,omitempty"`
	Cwd       string `json:"cwd,omitempty"`
	// Finished reports that the lane is resident but no longer working, which
	// is the case that forced the raw-herdr workaround.
	Finished bool     `json:"finished"`
	Text     string   `json:"text,omitempty"`
	Handoff  string   `json:"handoff,omitempty"`
	Commits  []string `json:"commits,omitempty"`
}

// ErrNoSuchAgent distinguishes "never existed or already closed" from a read
// failure. A fully closed tab has no pane, so its transcript is genuinely
// unavailable rather than empty; callers must not report that as a silent
// success.
type ErrNoSuchAgent struct {
	Name  string
	Known []string
}

func (e *ErrNoSuchAgent) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("no live agent %q, and no agents are resident; a fully closed tab has no readable pane", e.Name)
	}
	return fmt.Sprintf("no live agent %q; resident agents are: %s (a fully closed tab has no readable pane)",
		e.Name, strings.Join(e.Known, ", "))
}

// finishedStatuses are non-working resident states. A lane in one of these has
// produced whatever it is going to produce, which is exactly when a
// coordinator wants its handoff.
var finishedStatuses = map[string]bool{"done": true, "idle": true, "blocked": true}

// Read returns the transcript for one named agent. lines <= 0 selects a
// default deep enough to contain a normal completion report.
func Read(name string, lines int) (*Result, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, fmt.Errorf("herd transcript: agent name is required")
	}
	if lines <= 0 {
		lines = 200
	}
	agents, err := herdr.AgentList()
	if err != nil {
		return nil, fmt.Errorf("herd transcript: %w", err)
	}
	var found *herdr.AgentEntry
	known := make([]string, 0, len(agents))
	for i := range agents {
		if strings.TrimSpace(agents[i].Name) != "" {
			known = append(known, agents[i].Name)
		}
		if strings.EqualFold(strings.TrimSpace(agents[i].Name), name) {
			found = &agents[i]
		}
	}
	if found == nil {
		sort.Strings(known)
		return nil, &ErrNoSuchAgent{Name: name, Known: known}
	}

	out := &Result{
		Name: found.Name, Kind: found.Kind, Status: found.Status,
		PaneID: found.PaneID, TabID: found.TabID,
		Workspace: found.Workspace, Cwd: found.Cwd,
		Finished: finishedStatuses[strings.ToLower(strings.TrimSpace(found.Status))],
	}
	if strings.TrimSpace(found.PaneID) == "" {
		return out, fmt.Errorf("herd transcript: agent %q has no pane to read", name)
	}
	text, readErr := herdr.PaneRead(found.PaneID, lines)
	if readErr != nil {
		return out, fmt.Errorf("herd transcript: %w", readErr)
	}
	out.Text = text
	out.Handoff = ExtractHandoff(text)
	out.Commits = ExtractCommits(text)
	return out, nil
}

// reportMarker matches the bullet a lane uses to open its final report block.
var reportMarker = regexp.MustCompile(`(?m)^\s*[•*]\s+\S`)

// shaPattern matches a full or abbreviated git object name standing alone.
var shaPattern = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)

// ExtractHandoff isolates the lane's last reported block. Lane reports open
// with a bullet ("• BUILD COMPLETE FAC-547"), so the final bullet onward is
// the most recent report. When no marker is present the whole tail is returned
// rather than nothing: a coordinator reading a stuck lane still needs the text.
func ExtractHandoff(text string) string {
	trimmed := strings.TrimRight(text, "\n \t")
	if trimmed == "" {
		return ""
	}
	matches := reportMarker.FindAllStringIndex(trimmed, -1)
	if len(matches) == 0 {
		return ""
	}
	// Walk backwards to the last SUBSTANTIVE block. The final bullet is often
	// the harness's own activity line ("• Working (2m 36s ...)" or
	// "• Ran <command>"), which is not what a coordinator asked for; observed
	// live returning a spinner line instead of the lane's report.
	last := -1
	for i := len(matches) - 1; i >= 0; i-- {
		candidate := strings.TrimSpace(trimmed[matches[i][0]:])
		if isHarnessNoise(candidate) {
			continue
		}
		last = matches[i][0]
		break
	}
	if last < 0 {
		// Only noise present: return the newest line rather than nothing, so a
		// caller can still see the lane is mid-command.
		last = matches[len(matches)-1][0]
	}
	// Bullets delimit blocks, so the report ends where the next one begins.
	// Without this the chosen block ran to end-of-pane and swallowed every
	// later activity line.
	end := len(trimmed)
	for _, m := range matches {
		if m[0] > last {
			end = m[0]
			break
		}
	}
	block := strings.TrimSpace(trimmed[last:end])
	// Drop the harness's own input affordance, which is not part of the report.
	for _, noise := range []string{"\n›", "\nAsk ", "\n⏵"} {
		if idx := strings.Index(block, noise); idx > 0 {
			block = strings.TrimSpace(block[:idx])
		}
	}
	return block
}

// ExtractCommits returns object names mentioned in the text, in order, without
// duplicates. These are candidates a coordinator can verify -- never proof on
// their own that anything landed.
func ExtractCommits(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range shaPattern.FindAllString(text, -1) {
		// Require a plausible object length; 7 is git's short form.
		if len(m) < 7 || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	return out
}

// harnessNoisePrefixes open a harness activity line rather than a lane report.
var harnessNoisePrefixes = []string{
	"working (", "ran ", "added ", "edited ", "read ", "waiting for",
	"thinking", "explored", "searched", "listed",
}

// isHarnessNoise reports whether a bullet block is harness chrome. Matching is
// on the block's first line only: a real report may legitimately mention
// running a command in its body.
func isHarnessNoise(block string) bool {
	first := block
	if idx := strings.IndexByte(block, '\n'); idx >= 0 {
		first = block[:idx]
	}
	first = strings.ToLower(strings.TrimSpace(strings.TrimLeft(first, "•* \t")))
	for _, p := range harnessNoisePrefixes {
		if strings.HasPrefix(first, p) {
			return true
		}
	}
	return false
}
