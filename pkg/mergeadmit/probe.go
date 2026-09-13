package mergeadmit

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// FAC-162 incident: the live coordinator opened its PR#70 merge gate with
//
//	gh pr view ... | jq -r '...' | head -1
//
// `jq` and `head` both exit 0 over an empty stream, so a failed or
// unauthenticated `gh` produced an empty string and a zero exit — the gate
// read "no blocking condition" from a read that never happened.
//
// Probe makes that shape unrepresentable in compiled code. A probe returns a
// value AND its producer's own error, and Read refuses both a non-nil error
// and an empty value. There is no third outcome for a caller to mistake for
// success.

// Probe reads one piece of live state. Implementations MUST return the
// producing command's own error; they must never swallow it and return a zero
// value, and they must never return a value derived from a filter that ran
// after the producer failed.
type Probe func() (string, error)

// Read resolves a probe under fail-closed rules. name identifies the probe in
// the refusal so a rejection reason is actionable without a stack trace.
func (p Probe) Read(name string) (string, error) {
	if p == nil {
		return "", fmt.Errorf("live probe %q is not configured; admission cannot assume its value", name)
	}
	v, err := p()
	if err != nil {
		return "", fmt.Errorf("live probe %q failed: %w", name, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("live probe %q returned empty; an empty read is not evidence of an absent condition", name)
	}
	return strings.TrimSpace(v), nil
}

// OriginProbe reads the integration tip INSIDE the caller's allowance.
//
// FAC-831 review 6c93cc2b: the integration tip is not checkout hygiene. Which
// commit origin/main names is what the proof then proves and what the receipt
// then seals, so reading it is a proof operation and belongs inside the one
// finite context the public entry installs -- its deadline, its command and
// output budget, and its owned process group. The production probe fetches,
// and an unbounded fetch could hang or flood ahead of every budgeted command
// that follows it.
//
// Probe (no context) is kept for static and test wiring, which starts no
// process. This type is what production uses.
type OriginProbe func(ctx context.Context) (string, error)

// StaticOriginProbe is the context-aware form of a fixed reading. A static
// value starts no process, so it is safe at any budget; it exists so fixtures
// migrate to the required probe explicitly instead of being carried by a
// fallback.
func StaticOriginProbe(v string) OriginProbe {
	return func(context.Context) (string, error) { return v, nil }
}

// currentOriginMain is the ONE resolver for the integration tip on a PROOF
// route, and it REQUIRES the context-aware probe.
//
// There is deliberately no fallback to the value-only Probe. That field holds
// an arbitrary closure, and an arbitrary closure can shell out unbounded --
// cmd/herd wired exactly that, twice. A silent fallback would mean a proof
// route could still reach an unbounded command while every visible bound said
// otherwise, so a missing context-aware probe is a REFUSAL and the caller is
// told which field to set. Static readings are fine; hidden context-free
// executables are not.
func (g *Gate) currentOriginMain(ctx context.Context, role string) (string, error) {
	if g == nil || g.Live.OriginMainAt == nil {
		return "", fmt.Errorf(
			"live probe %q is not configured for a proof route: set LiveState.OriginMainAt, "+
				"which reads the integration tip inside this invocation's allowance; "+
				"the value-only OriginMain field is not accepted here because it can run unbounded commands",
			role)
	}
	v, err := g.Live.OriginMainAt(ctx)
	if err != nil {
		return "", fmt.Errorf("live probe %q failed: %w", role, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", fmt.Errorf("live probe %q returned empty; an empty read is not evidence of an absent condition", role)
	}
	return strings.TrimSpace(v), nil
}

// CurrentOriginMain exposes the resolver so a caller that composes an operation
// -- the CLI verify-landed route -- observes the tip through the GATE'S injected
// probe on the shared context, instead of constructing a second origin path of
// its own.
func (g *Gate) CurrentOriginMain(ctx context.Context, role string) (string, error) {
	return g.currentOriginMain(ctx, role)
}

// CommandProbe builds a Probe from a single command with NO shell and NO
// pipeline. The exit status that reaches the caller is the producer's own,
// because there is no downstream filter to overwrite it.
//
// Post-processing belongs in Go, applied to the output only after the error
// has already been checked — which is exactly what this function does.
func CommandProbe(dir, name string, args []string, extract func(string) (string, error)) Probe {
	return func() (string, error) {
		cmd := exec.Command(name, args...)
		cmd.Dir = dir
		out, err := cmd.Output()
		// The producer's status is checked BEFORE the output is looked at.
		// Reversing these two lines reintroduces the FAC-162 bug.
		if err != nil {
			return "", fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
		}
		if extract == nil {
			return string(out), nil
		}
		return extract(string(out))
	}
}

// StaticProbe returns a fixed value. Used by hermetic tests and by callers
// that already hold an authoritatively-read value; it is not a way to assert
// something that was never read.
func StaticProbe(v string) Probe { return func() (string, error) { return v, nil } }

// FailingProbe returns a fixed error. Used by tests to prove a failed producer
// cannot be mistaken for an absent condition.
func FailingProbe(err error) Probe { return func() (string, error) { return "", err } }

// LiveState is everything admission must re-read immediately before a merge.
// Every field is a Probe, so every one of them fails closed.
//
// This is the "re-read current state right before merging" requirement: a
// value captured minutes earlier during review is not current state, and the
// serial integration train invalidates receipts precisely by advancing these.
type LiveState struct {
	// OriginMain is the exact current integration base. Static and test wiring
	// use this; it starts no process.
	OriginMain Probe
	// OriginMainAt is the same reading, taken INSIDE the caller's allowance.
	// Production sets this so the fetch and rev-parse that decide the landed
	// commit are charged, deadlined and owned like every other proof command.
	// When set it is preferred over OriginMain.
	OriginMainAt OriginProbe
	// CandidateHead is the exact current tip of the candidate being merged
	// (the PR head), read from the authority that owns it.
	CandidateHead Probe
	// Mergeable is the provider's mergeability state (e.g. "CLEAN").
	Mergeable Probe
	// TaskRevision is the current board task revision, which the acceptance
	// digest is bound to. A card edited after review is a different card.
	TaskRevision Probe
	// Checks reports the state of every required CI check by name. A missing
	// name is a refusal, not a pass.
	Checks ChecksProbe
}

// ChecksProbe reads required-CI state. It returns a name→conclusion map and
// the producer's own error; an empty map with a nil error is refused by the
// caller, since "no checks reported" is how a failed API read looks.
type ChecksProbe func() (map[string]string, error)

// checkPassed is the closed set of conclusions that count as green. Anything
// else — pending, empty, skipped, cancelled, or a state this build has never
// heard of — is not green. An unknown conclusion must never widen the gate.
var checkPassed = map[string]bool{
	"success": true,
	"neutral": true,
}

// readChecks resolves the required checks by name and refuses on the first one
// that is missing or not green.
func readChecks(p ChecksProbe, required []string) (map[string]string, error) {
	if p == nil {
		return nil, fmt.Errorf("required-CI probe is not configured; admission cannot assume checks are green")
	}
	got, err := p()
	if err != nil {
		return nil, fmt.Errorf("required-CI probe failed: %w", err)
	}
	if len(got) == 0 {
		return nil, fmt.Errorf("required-CI probe reported no checks at all; an empty check set is not a green check set")
	}
	// Case-insensitive lookup: providers vary the casing of a check name and a
	// case mismatch must not read as "check absent".
	byLower := make(map[string]string, len(got))
	for k, v := range got {
		byLower[strings.ToLower(strings.TrimSpace(k))] = strings.ToLower(strings.TrimSpace(v))
	}
	for _, name := range required {
		key := strings.ToLower(strings.TrimSpace(name))
		if key == "" {
			continue
		}
		conclusion, ok := byLower[key]
		if !ok {
			return nil, fmt.Errorf("required check %q was not reported; a check that did not run has not passed", name)
		}
		if !checkPassed[conclusion] {
			return nil, fmt.Errorf("required check %q concluded %q, which is not green", name, conclusion)
		}
	}
	return byLower, nil
}
