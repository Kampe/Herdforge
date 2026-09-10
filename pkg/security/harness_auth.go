package security

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"time"
)

// FAC-576: some harnesses authenticate THEMSELVES. They hold a session created
// by their own CLI login and never present a brokered host credential, so asking
// whether a handle is installed for their API host is the wrong question — and
// answering it blocked every claude reviewer launch on this fleet with a demand
// for an api.anthropic.com handle claude will never use.

// harnessAuthKinds are the kinds that authenticate through their own harness.
//
// FAC-587, FAC-791: that is EVERY kind this fleet launches. Agents run as
// harnesses inside herdr panes and never against a provider API, so codex, grok,
// and opencode hold their own CLI login sessions and configuration exactly as
// claude and agy do. Listing only some harnesses left opencode classified as an
// API-key kind and refused at admission for credentials this fleet does not
// have — the same category error FAC-576 fixed for claude and FAC-587 fixed for
// the others.
//
// If this fleet ever adopts raw API execution, the change belongs in
// RequestRulesForKind and RequiredBrokerHostsForKind behind an explicit opt-in,
// not by reclassifying a kind here.
var harnessAuthKinds = map[string]bool{
	AuthorKindAGY:      true, "antigravity": true,
	AuthorKindClaude:   true,
	AuthorKindCodex:    true,
	AuthorKindGrok:     true,
	AuthorKindOpenCode: true,
}

// harnessAuthenticated reports whether a kind carries its own session.
func harnessAuthenticated(kind string) bool {
	return harnessAuthKinds[strings.ToLower(strings.TrimSpace(kind))]
}

// HarnessLogin is a harness's own view of whether it is signed in.
type HarnessLogin string

const (
	// HarnessLoggedIn means the harness reported an active session.
	HarnessLoggedIn HarnessLogin = "logged-in"
	// HarnessLoggedOut means the harness reported no session. This is a real
	// blocker: the pane will land on a login screen.
	HarnessLoggedOut HarnessLogin = "logged-out"
	// HarnessLoginUnknown means the question could not be asked — the CLI is
	// absent, too old to answer, or timed out. Deliberately distinct from
	// logged-out: inability to ask is not evidence of absence, and treating it
	// as a blocker would refuse healthy hosts whose CLI simply lacks the
	// subcommand.
	HarnessLoginUnknown HarnessLogin = "unknown"
)

// harnessLoginProbeTimeout bounds the probe. It is on the preflight path, so a
// hung CLI must not hang the gate.
const harnessLoginProbeTimeout = 8 * time.Second

// harnessLoginProbeHook allows tests to override the status probe for harness auth.
// When nil, the default CLI execution is used.
var harnessLoginProbeHook func(ctx context.Context, kind string) ([]byte, error)

// SetHarnessLoginProbeForTesting sets a hook for testing harness login probes.
// It returns a cleanup function that restores the previous hook.
func SetHarnessLoginProbeForTesting(fn func(ctx context.Context, kind string) ([]byte, error)) func() {
	prev := harnessLoginProbeHook
	harnessLoginProbeHook = fn
	return func() { harnessLoginProbeHook = prev }
}

// HarnessLoginState asks a harness whether it is signed in.
//
// It reads ONLY the boolean. The response also carries an email, an org id and a
// subscription type, and none of that belongs in a diagnosis that gets printed
// and logged.
func HarnessLoginState(kind string) HarnessLogin {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if !harnessAuthenticated(kind) {
		return HarnessLoginUnknown
	}
	// Only claude exposes a machine-readable session check today. agy has no
	// equivalent, and inventing one from parsing prose would be a guess.
	if kind != AuthorKindClaude {
		return HarnessLoginUnknown
	}
	var out []byte
	var err error
	if harnessLoginProbeHook != nil {
		ctx, cancel := context.WithTimeout(context.Background(), harnessLoginProbeTimeout)
		defer cancel()
		out, err = harnessLoginProbeHook(ctx, kind)
	} else {
		if _, lookErr := exec.LookPath("claude"); lookErr != nil {
			return HarnessLoginUnknown
		}
		ctx, cancel := context.WithTimeout(context.Background(), harnessLoginProbeTimeout)
		defer cancel()
		out, err = exec.CommandContext(ctx, "claude", "auth", "status", "--json").Output()
	}
	if err != nil {
		return HarnessLoginUnknown
	}
	var status struct {
		LoggedIn *bool `json:"loggedIn"`
	}
	if jsonErr := json.Unmarshal(out, &status); jsonErr != nil || status.LoggedIn == nil {
		return HarnessLoginUnknown
	}
	if *status.LoggedIn {
		return HarnessLoggedIn
	}
	return HarnessLoggedOut
}
