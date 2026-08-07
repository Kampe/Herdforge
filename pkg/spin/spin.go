// Package spin ports bin/herd-spin: detect stalled or spinning agent panes.
//
// Two distinct failures look identical from the outside — both show
// status=working forever — so they need different signals:
//
//	STALL: the pane's output is frozen. Detected by fingerprinting the tail
//	       with timer/cost/spinner noise stripped, because a "thinking" pane
//	       redraws a ticking counter forever and would otherwise never look
//	       frozen.
//	SPIN:  the agent is producing output but making no progress — no HEAD
//	       change and no dirty-count change. Only meaningful for WRITER-class
//	       agents: research and coordinator work legitimately produces no git
//	       delta, so applying it there would fire constantly.
//	LONG:  advisory only. Working wall-time past a per-class norm.
package spin

import (
	"crypto/md5"
	"encoding/hex"
	"regexp"
	"strings"
	"time"
)

// Finding is one detected condition.
type Finding string

const (
	Stall Finding = "STALL"
	Spin  Finding = "SPIN"
	Long  Finding = "LONG"
)

// Defaults mirror the shell's tunables.
const (
	DefaultInterval     = 180 * time.Second
	DefaultStallSamples = 2
	DefaultSpinSamples  = 4
	DefaultLongReview   = 10 * time.Minute
	DefaultLongFix      = 15 * time.Minute
	DefaultLongBuild    = 30 * time.Minute
)

var (
	ansiRe    = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)
	spinnerRe = regexp.MustCompile(`^[\s⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏◐◓◑◒⠂⠄⠆⠃⠁⠈⠐⠠⢀⡀]*$`)
	tokensRe  = regexp.MustCompile(`(?i)[0-9]+\s*tokens?|tokens?\s*[:=0-9]`)
	contextRe = regexp.MustCompile(`(?i)context\s*[0-9]+%`)
	escRe     = regexp.MustCompile(`(?i)esc to interrupt`)
	timerRe   = regexp.MustCompile(`^\s*[0-9]+s\s*$`)
	wsRe      = regexp.MustCompile(`\s+`)
)

// NormalizeTail strips the noise that changes every frame even when an agent
// is doing nothing: spinners, token counters, context percentages, ticking
// timers, and ANSI escapes. Without this, a frozen pane never fingerprints as
// frozen and STALL can never fire.
func NormalizeTail(tail string) string {
	var kept []string
	for _, line := range strings.Split(tail, "\n") {
		line = ansiRe.ReplaceAllString(line, "")
		switch {
		case spinnerRe.MatchString(line),
			tokensRe.MatchString(line),
			contextRe.MatchString(line),
			escRe.MatchString(line),
			timerRe.MatchString(line):
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(wsRe.ReplaceAllString(strings.Join(kept, " "), " "))
}

// Fingerprint is the comparable identity of a pane's output.
func Fingerprint(tail string) string {
	sum := md5.Sum([]byte(NormalizeTail(tail)))
	return hex.EncodeToString(sum[:])
}

// Class buckets an agent for the LONG advisory.
type Class string

const (
	ClassReview   Class = "review"
	ClassFix      Class = "fix"
	ClassResearch Class = "research"
	ClassBuild    Class = "build"
)

// ClassFor buckets by agent name.
func ClassFor(name string) Class {
	n := strings.ToLower(name)
	switch {
	case strings.Contains(n, "review"), strings.Contains(n, "assayer"):
		return ClassReview
	case strings.Contains(n, "fix"), strings.Contains(n, "hotfix"):
		return ClassFix
	case strings.Contains(n, "scout"), strings.Contains(n, "planner"), strings.Contains(n, "docs"):
		return ClassResearch
	}
	return ClassBuild
}

// LongLimit is the working wall-time past which LONG is advised.
func LongLimit(c Class) time.Duration {
	switch c {
	case ClassReview:
		return DefaultLongReview
	case ClassFix:
		return DefaultLongFix
	case ClassResearch:
		// Research legitimately runs long; double the build norm.
		return DefaultLongBuild * 2
	}
	return DefaultLongBuild
}

// IsWriter reports whether git-delta SPIN may fire for this agent. Any agent
// working inside a worktree is writer-class; coordinator, planner and docs
// roles are not, because their work produces no git delta by design.
func IsWriter(name, cwd string) bool {
	n := strings.ToLower(name)
	for _, nonWriter := range []string{"orchestrator", "coordinator", "scout-planner", "docs", "supervisor"} {
		if strings.Contains(n, nonWriter) {
			return false
		}
	}
	return strings.Contains(cwd, "/worktrees/")
}

// Sample is one observation of a pane.
type Sample struct {
	PaneID      string `json:"pane_id"`
	Name        string `json:"name"`
	AgentStatus string `json:"agent_status"`
	Fingerprint string `json:"fingerprint"`
	Head        string `json:"head,omitempty"`
	Dirty       int    `json:"dirty"`
	Writer      bool   `json:"writer"`
	StallHits   int    `json:"stall_hits"`
	SpinHits    int    `json:"spin_hits"`
	// FirstWorkingUnix is when this pane was first seen working; it resets
	// whenever the pane leaves the working state.
	FirstWorkingUnix int64 `json:"first_working_unix"`

	// The fields below belong to the FAC-90 durable progress assessment (see
	// progress.go). They live on Sample so one persisted file carries both
	// the diagnostic counters above and the act budget below — the budget is
	// only a rate limit if it survives a restart.
	Progress         Progress `json:"progress"`
	PID              int      `json:"pid,omitempty"`
	NoProgressCycles int      `json:"no_progress_cycles"`
	RestartCycles    int      `json:"restart_cycles"`
	// ActsUnix are the times spin acted on this pane, newest last, trimmed
	// to Policy.ActWindow.
	ActsUnix []int64 `json:"acts_unix,omitempty"`
	// LastActionTaken is the last act actually performed, so a second stall
	// escalates from nudge to recovery instead of nudging forever.
	LastActionTaken Action `json:"last_action_taken,omitempty"`
}

// Thresholds tune detection.
type Thresholds struct {
	StallSamples int
	SpinSamples  int
}

// DefaultThresholds returns the shell defaults.
func DefaultThresholds() Thresholds {
	return Thresholds{StallSamples: DefaultStallSamples, SpinSamples: DefaultSpinSamples}
}

// Classify folds a new observation into the prior sample and reports findings.
// It returns the updated sample so the caller can persist it.
//
// Counters reset the moment anything moves — output changing, HEAD advancing,
// or the dirty count shifting — so a slow-but-live agent never accumulates
// toward a false STALL.
func Classify(prev *Sample, now Sample, th Thresholds, workingFor time.Duration) (Sample, []Finding) {
	out := now
	if prev == nil || !strings.EqualFold(prev.AgentStatus, "working") {
		out.StallHits, out.SpinHits = 0, 0
	} else {
		if prev.Fingerprint == now.Fingerprint && now.Fingerprint != "" {
			out.StallHits = prev.StallHits + 1
		} else {
			out.StallHits = 0
		}
		if now.Writer && prev.Head == now.Head && prev.Dirty == now.Dirty {
			out.SpinHits = prev.SpinHits + 1
		} else {
			out.SpinHits = 0
		}
	}

	// Only a working pane can stall or spin. An idle or blocked pane is a
	// different problem and reporting it here would drown the real signal.
	if !strings.EqualFold(now.AgentStatus, "working") {
		out.StallHits, out.SpinHits = 0, 0
		return out, nil
	}

	var findings []Finding
	if out.StallHits >= th.StallSamples {
		findings = append(findings, Stall)
	}
	if out.Writer && out.SpinHits >= th.SpinSamples {
		findings = append(findings, Spin)
	}
	if workingFor > LongLimit(ClassFor(now.Name)) {
		findings = append(findings, Long)
	}
	return out, findings
}
