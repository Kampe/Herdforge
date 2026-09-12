package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

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
	processPaneRead       = herdr.PaneReadContext
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
	// Deadline bounds the whole sweep. The single sweep context carrying it is
	// passed through the roster read AND every pane read, so a read that hangs
	// is cancelled inside the child rather than merely skipped afterwards.
	Deadline time.Duration
}

// processDigestEnvelope is the JSON surface: the existing Digest fields
// verbatim, plus additive machine-readable partial/error information. The
// original keys are unchanged so existing consumers keep working, and a
// consumer that only reads them still sees exactly what it saw before — but
// nothing forces an operator to parse stderr to learn the sweep was incomplete.
type processDigestEnvelope struct {
	process.Digest
	Partial   bool     `json:"partial,omitempty"`
	PaneReads int      `json:"pane_reads"`
	Skipped   int      `json:"skipped,omitempty"`
	Unknowns  []string `json:"unknowns,omitempty"`
}

func newProcessDigestEnvelope(result processScanResult) processDigestEnvelope {
	return processDigestEnvelope{
		Digest:    result.Digest,
		Partial:   result.Partial,
		PaneReads: result.PaneReads,
		Skipped:   result.Skipped,
		Unknowns:  result.Unknowns,
	}
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

// transportFailureReason names a TRANSPORT failure, as distinct from an agent
// whose own output merely looks like one.
//
// An earlier version inspected the decoded AGENT TEXT for an "error" key, which
// is the wrong layer in both directions: a healthy pane whose literal output is
// {"error":"example"} was mislabelled a tool failure, while a real error
// envelope carrying a result array was consumed as data. The distinction now
// lives in pkg/herdr, where the envelope is decoded before any agent text is
// extracted, and arrives here as a typed error.
func transportFailureReason(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, herdr.ErrReadTransportContradiction):
		return "pane read returned an error and a result together", true
	case errors.Is(err, herdr.ErrReadTransportEnvelope):
		return "pane read returned a transport error envelope", true
	case errors.Is(err, herdr.ErrReadTransportOversized):
		return "pane read exceeded the transport byte bound and was stopped", true
	case errors.Is(err, herdr.ErrReadTransportMalformed):
		return "pane read returned a structured response that could not be decoded", true
	case errors.Is(err, herdr.ErrReadTransportUnsupportedResult):
		return "pane read returned an unsupported result body", true
	}
	return "", false
}

// maxProcessPaneLines is the ceiling on user-supplied tail depth. --lines may
// narrow the default, never widen past it: an arbitrary positive integer would
// otherwise walk straight past the bound the sweep is supposed to hold.
const maxProcessPaneLines = 50

// maxUnknownDetailBytes bounds the transport message carried alongside a
// failure reason. The reason names the CLASS of failure; the detail is herdr's
// own message, and it is often the only clue an operator has about WHY a pane
// could not be read. Dropping it to keep the digest tidy would be tidiness
// bought with the operator's diagnosis.
const maxUnknownDetailBytes = 240

// errDetail renders an error as one bounded, single-line, valid-UTF-8 string.
func errDetail(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if len(msg) <= maxUnknownDetailBytes {
		return msg
	}
	cut := msg[:maxUnknownDetailBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut + "..."
}

var errProcessBadLines = errors.New("herd process: --lines must be a positive integer")

// resolveProcessLines validates and caps the requested depth.
func resolveProcessLines(requested, def int) (int, bool, error) {
	if requested == 0 {
		return def, false, nil
	}
	if requested < 0 {
		return 0, false, errProcessBadLines
	}
	if requested > maxProcessPaneLines {
		return maxProcessPaneLines, true, nil
	}
	return requested, false, nil
}

// truncateTail bounds one pane's text before classification, keeping the END
// where a verdict appears.
//
// The cut is moved back to a rune boundary. Slicing bytes mid-rune produces
// invalid UTF-8, which json.Marshal silently rewrites to U+FFFD — a quiet
// corruption of the operator's evidence. If a boundary cannot be found within
// the cap the text is reported as unusable rather than emitted corrupt.
func truncateTail(text string, maxBytes int) (string, bool) {
	if maxBytes <= 0 || len(text) <= maxBytes {
		return text, false
	}
	cut := text[len(text)-maxBytes:]
	for i := 0; i < len(cut) && i < utf8.UTFMax; i++ {
		if utf8.ValidString(cut[i:]) {
			return cut[i:], true
		}
	}
	return "", true
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
		observation, readErr := processPaneRead(ctx, agent.PaneID, limits.Lines)
		text := observation.Text
		switch {
		case readErr != nil:
			reason, isTransport := transportFailureReason(readErr)
			if !isTransport {
				reason = "pane read failed"
			}
			result.Partial = true
			note := fmt.Sprintf("%s: %s", agent.Name, reason)
			if detail := errDetail(readErr); detail != "" {
				note = fmt.Sprintf("%s (%s)", note, detail)
			}
			result.Unknowns = append(result.Unknowns, note)
			text = ""
		default:
			// A reply that never claimed to be a structured herdr response is
			// usable text with nothing behind it. It is classified — dropping
			// it would lose real evidence — but the digest says so and does
			// not call itself complete. A silent healthy classification from
			// an unverified payload is exactly the failure mode the envelope
			// validation exists to prevent.
			if observation.Unverified {
				result.Partial = true
				result.Unknowns = append(result.Unknowns,
					fmt.Sprintf("%s: pane text was not a herdr transport response; classification is unverified", agent.Name))
			}
			// herdr's OWN truncation flag, distinct from this sweep's cap: the
			// tail is short because herdr cut it, and a verdict may be missing
			// for that reason alone.
			if observation.Truncated {
				result.Partial = true
				result.Unknowns = append(result.Unknowns,
					fmt.Sprintf("%s: herdr reported the pane tail as truncated", agent.Name))
			}
			var truncated bool
			text, truncated = truncateTail(text, limits.MaxTailBytes)
			if truncated {
				// EVERY sweep-caused truncation is incomplete, not only the
				// case where no rune boundary was found. Classifying a tail
				// this sweep cut is classifying evidence it knows is missing:
				// the verdict may have been in the bytes that were dropped.
				// Reporting it in Unknowns while still exiting 0 is the exact
				// shape of "nothing needs attention" being believed, and it is
				// the same fact herdr states with its own truncated flag.
				result.Partial = true
				note := fmt.Sprintf("%s: pane tail truncated to %d bytes by this sweep", agent.Name, limits.MaxTailBytes)
				if text == "" {
					// No rune boundary inside the cap: report incomplete rather
					// than hand the classifier corrupt bytes.
					note = fmt.Sprintf("%s: pane tail incomplete; no UTF-8 boundary within %d bytes", agent.Name, limits.MaxTailBytes)
				}
				result.Unknowns = append(result.Unknowns, note)
			}
		}
		// Classification stays advisory and unchanged: the same function the
		// unit tests already cover, fed real text instead of a fixture. An
		// unreadable pane reaches it with empty text, so it classifies UNKNOWN
		// regardless of whatever the roster's status field still claims.
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

// maxProcessDeadline is the ceiling on the sweep bound. --deadline exists so an
// operator (and the subprocess fixtures) can tighten the sweep; it may never
// widen it past this, because the point of the bound is that it is finite and
// known, not caller-supplied.
const maxProcessDeadline = 5 * time.Minute

var errProcessBadDeadline = errors.New("herd process: --deadline must be a positive duration")

// resolveProcessDeadline validates and caps the requested sweep bound. The
// returned duration always satisfies 0 < d <= maxProcessDeadline, so the single
// sweep context handed to the roster read and every pane read is finite on
// every path through the flag.
func resolveProcessDeadline(requested, def time.Duration) (time.Duration, bool, error) {
	if requested == 0 {
		return def, false, nil
	}
	if requested < 0 {
		return 0, false, errProcessBadDeadline
	}
	if requested > maxProcessDeadline {
		return maxProcessDeadline, true, nil
	}
	return requested, false, nil
}
