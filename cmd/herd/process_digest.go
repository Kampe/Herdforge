package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/process"
)

// FAC-36: `herd process` classified a hard-coded pane-demo/agent-demo record,
// so its JSON was a fixture that happened to exit 0. The classifiers in
// pkg/process were real and unit tested; nothing proved the CLI ever fed them a
// live input. This file is the missing adapter: it reads the real roster and
// real pane text through herdr's existing READ-ONLY Go client and hands the
// text to the same advisory classifiers.
//
// What it deliberately does not do: mutate anything, send to a pane, interrupt
// an agent, run a provider call, invoke a mutating harvest, or shell out to a
// wrapper. A classification is advisory evidence about text, never authority to
// act on the agent it describes.

// Injectable seams. Production wires herdr's read-only surfaces; hermetic tests
// replace them to exercise roster and pane-read behaviour without a live
// terminal. They are variables rather than an interface because there is one
// production implementation and one test implementation.
var (
	processHerdrAvailable = herdr.IsAvailable
	processAgentList      = herdr.AgentListContext
	processPaneRead       = herdr.PaneRead
)

// processScanLimits bounds the sweep. Every field is a hard ceiling, not a
// hint: an unbounded fleet scan is how a read-only digest becomes a load
// problem on a host that is already under pressure.
type processScanLimits struct {
	// MaxPanes caps how many panes are read in one sweep.
	MaxPanes int
	// Lines is the pane tail depth requested per pane.
	Lines int
	// MaxTailBytes truncates each pane tail before classification.
	MaxTailBytes int
	// Deadline bounds the whole sweep. It is checked before each pane read;
	// herdr.PaneRead itself takes no context, so this stops issuing further
	// reads rather than cancelling one already in flight.
	Deadline time.Duration
}

func defaultProcessScanLimits() processScanLimits {
	return processScanLimits{MaxPanes: 64, Lines: 50, MaxTailBytes: 16 << 10, Deadline: 30 * time.Second}
}

// processScanResult carries the digest plus what the sweep could NOT establish.
// Unknowns are reported, never folded into a clean result: a pane that could
// not be read is not a pane with nothing to say.
type processScanResult struct {
	Digest process.Digest
	// PaneReads is the number of pane reads actually issued. Tests assert on it
	// so an accidental unbounded or repeated scan cannot pass unnoticed.
	PaneReads int
	// Skipped counts in-scope panes never read, because the cap or the deadline
	// was reached first.
	Skipped int
	// Unknowns are per-pane failures in scan order, as short reasons.
	Unknowns []string
	// Partial is true when the digest does not describe every in-scope pane.
	Partial bool
}

var errProcessNoWorkspace = errors.New("herd process: no workspace scope; pass --workspace or set fleet.herdr_workspace")

// resolveProcessWorkspace makes the scope explicit. An unscoped sweep would
// read panes belonging to other repositories' fleets and label the result with
// an empty workspace, which is both a wider read than asked for and an
// untrustworthy digest. Refusing is the honest default.
func resolveProcessWorkspace(flagValue, configured string) (string, error) {
	if ws := strings.TrimSpace(flagValue); ws != "" {
		return ws, nil
	}
	if ws := strings.TrimSpace(configured); ws != "" {
		return ws, nil
	}
	return "", errProcessNoWorkspace
}

// isToolErrorEnvelope reports whether pane text is a tool's JSON error payload
// rather than agent output.
//
// herdr.PaneRead falls back to returning the raw body when it cannot decode a
// known envelope, so an error envelope arrives looking exactly like agent text.
// Classifying it would let a TOOL failure be read as an AGENT verdict — the
// error string mentioning "failed" would classify FAIL for an agent that may be
// perfectly healthy.
func isToolErrorEnvelope(text string) bool {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "{") {
		return false
	}
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		OK     *bool           `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if json.Unmarshal([]byte(trimmed), &envelope) != nil {
		return false
	}
	if len(envelope.Error) > 0 && string(envelope.Error) != "null" {
		return true
	}
	if envelope.OK != nil && !*envelope.OK {
		return true
	}
	return false
}

// truncateTail bounds one pane's text. The cap is applied before
// classification so a runaway pane cannot make the digest unbounded.
func truncateTail(text string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text, false
	}
	return text[len(text)-maxBytes:], true
}

// inScope reports whether an agent belongs to the requested workspace. An agent
// whose workspace is unreported is NOT assumed to be in scope: guessing would
// silently widen the sweep.
func inScope(agent herdr.AgentEntry, workspace string) bool {
	return strings.TrimSpace(agent.Workspace) == workspace
}

// collectProcessDigest performs one bounded read-only sweep.
//
// Failure modes are kept distinct on purpose. An unavailable herdr, or a roster
// that cannot be read or parsed, is an ERROR: a broken roster must never
// present as an empty fleet, because "nothing needs attention" and "I could not
// look" are opposite operational facts. A genuinely empty in-scope fleet is a
// success with zero items.
func collectProcessDigest(ctx context.Context, workspace string, limits processScanLimits) (processScanResult, error) {
	var result processScanResult
	result.Digest = process.Digest{WorkspaceID: workspace, Items: []process.Target{}}

	if !processHerdrAvailable() {
		return result, errors.New("herd process: herdr is not available on PATH; refusing to report an empty fleet")
	}
	agents, err := processAgentList(ctx)
	if err != nil {
		return result, fmt.Errorf("herd process: roster unreadable: %w", err)
	}

	scoped := make([]herdr.AgentEntry, 0, len(agents))
	for _, agent := range agents {
		if inScope(agent, workspace) {
			scoped = append(scoped, agent)
		}
	}
	// Deterministic order so a digest is comparable between runs and a capped
	// sweep always drops the same tail rather than an arbitrary one.
	sort.Slice(scoped, func(i, j int) bool {
		if scoped[i].Name != scoped[j].Name {
			return scoped[i].Name < scoped[j].Name
		}
		return scoped[i].PaneID < scoped[j].PaneID
	})

	deadline := time.Now().Add(limits.Deadline)
	for i := range scoped {
		agent := scoped[i]
		if limits.MaxPanes > 0 && result.PaneReads >= limits.MaxPanes {
			result.Skipped = len(scoped) - i
			result.Partial = true
			result.Unknowns = append(result.Unknowns,
				fmt.Sprintf("pane cap %d reached; %d in-scope pane(s) not read", limits.MaxPanes, result.Skipped))
			break
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			result.Skipped = len(scoped) - i
			result.Partial = true
			result.Unknowns = append(result.Unknowns,
				fmt.Sprintf("sweep canceled (%v); %d in-scope pane(s) not read", ctxErr, result.Skipped))
			break
		}
		if !time.Now().Before(deadline) {
			result.Skipped = len(scoped) - i
			result.Partial = true
			result.Unknowns = append(result.Unknowns,
				fmt.Sprintf("sweep deadline %s reached; %d in-scope pane(s) not read", limits.Deadline, result.Skipped))
			break
		}
		if strings.TrimSpace(agent.PaneID) == "" {
			result.Partial = true
			result.Unknowns = append(result.Unknowns, fmt.Sprintf("%s: no pane id reported", agent.Name))
			result.Digest.Items = append(result.Digest.Items,
				process.ClassifyTarget("", agent.Name, agent.Status, ""))
			continue
		}

		result.PaneReads++
		text, readErr := processPaneRead(agent.PaneID, limits.Lines)
		switch {
		case readErr != nil:
			result.Partial = true
			result.Unknowns = append(result.Unknowns, fmt.Sprintf("%s: pane read failed", agent.Name))
			text = ""
		case isToolErrorEnvelope(text):
			result.Partial = true
			result.Unknowns = append(result.Unknowns, fmt.Sprintf("%s: pane read returned a tool error envelope", agent.Name))
			text = ""
		default:
			var truncated bool
			text, truncated = truncateTail(text, limits.MaxTailBytes)
			if truncated {
				result.Unknowns = append(result.Unknowns, fmt.Sprintf("%s: pane tail truncated to %d bytes", agent.Name, limits.MaxTailBytes))
			}
		}
		// Classification stays advisory and unchanged: the same function the
		// unit tests already cover, fed real text instead of a fixture.
		result.Digest.Items = append(result.Digest.Items,
			process.ClassifyTarget(agent.PaneID, agent.Name, agent.Status, text))
	}
	return result, nil
}

// renderProcessDigestText is the human format. It states what was not read as
// plainly as what was, so a partial sweep never reads like a complete one.
func renderProcessDigestText(result processScanResult) string {
	var b strings.Builder
	fmt.Fprintf(&b, "workspace %s: %d agent(s), %d pane read(s)\n",
		orEmpty(result.Digest.WorkspaceID), len(result.Digest.Items), result.PaneReads)
	for _, item := range result.Digest.Items {
		fmt.Fprintf(&b, "  %-10s %-28s %s\n", item.Class, item.Name, item.Action)
	}
	if len(result.Digest.Items) == 0 {
		fmt.Fprintf(&b, "  (no agents in scope)\n")
	}
	for _, unknown := range result.Unknowns {
		fmt.Fprintf(&b, "  UNKNOWN    %s\n", unknown)
	}
	if result.Partial {
		fmt.Fprintf(&b, "PARTIAL: this digest does not describe every in-scope pane\n")
	}
	return b.String()
}
