package router

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Advertising probe helpers remain for optional bounded live checks and for
// hermetic regression of the CHA-2451 hang class. ReadPathProbes (resolve-lane /
// herd route) skips live generation entirely: CLIPresent + ProbeSurface + quota
// stay honest, and a stuck codex spark child cannot silence JSON output.
const (
	AdvertisingProbeTimeout  = 2 * time.Second
	advertisingProbeCacheTTL = 30 * time.Second
)

type advertisingCacheEntry struct {
	ok     bool
	reason string
	at     time.Time
}

var advertisingProbeCache = struct {
	mu sync.Mutex
	m  map[string]advertisingCacheEntry
}{m: map[string]advertisingCacheEntry{}}

func resetAdvertisingProbeCacheForTest() {
	advertisingProbeCache.mu.Lock()
	advertisingProbeCache.m = map[string]advertisingCacheEntry{}
	advertisingProbeCache.mu.Unlock()
}

// advertisingProviderProbe is a short-deadline cached live readiness check.
// ReadPathProbes does not install it by default (CHA-2451 skip path).
func advertisingProviderProbe(provider, model string) (bool, string) {
	key := ProbeKey(provider, model)
	advertisingProbeCache.mu.Lock()
	if e, ok := advertisingProbeCache.m[key]; ok && time.Since(e.at) < advertisingProbeCacheTTL {
		ok, reason := e.ok, e.reason
		advertisingProbeCache.mu.Unlock()
		return ok, reason
	}
	advertisingProbeCache.mu.Unlock()

	ok, reason := runProviderProbe(provider, model, AdvertisingProbeTimeout)
	advertisingProbeCache.mu.Lock()
	advertisingProbeCache.m[key] = advertisingCacheEntry{ok: ok, reason: reason, at: time.Now()}
	advertisingProbeCache.mu.Unlock()
	return ok, reason
}

// runProviderProbe executes one provider readiness request under deadline.
// timeout <= 0 falls back to the launch-path budget (45s).
func runProviderProbe(provider, model string, timeout time.Duration) (bool, string) {
	if timeout <= 0 {
		timeout = providerProbeBudget
	}
	command, args, stdin, err := providerProbeCommand(provider, model)
	if err != nil {
		return false, err.Error()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	started := time.Now()
	stdout, stderr, runErr, timedOut := execProviderProbe(ctx, command, args, stdin)
	elapsed := time.Since(started)
	combined := stdout + "\n" + stderr
	ok, reason := classifyProviderProbeOutput(stdout, combined, runErr, timedOut)
	if timedOut && reason == probeTimeoutMarker {
		// CHA-2451's read paths keep their terse machine marker; they already
		// skip the live probe and only need a deadline token.
		if timeout <= AdvertisingProbeTimeout {
			return false, "provider_probe_deadline"
		}
		// FAC-596: the admission path surfaces this string to an operator as
		// the routing reason, so it must say WHAT it waited on, HOW LONG, and
		// that the answer is unknown rather than a refusal.
		return false, fmt.Sprintf("provider probe %s: %s/%s did not answer within %s (waited %s)",
			ProbeUnknownPrefix, provider, model, timeout.Round(time.Second), elapsed.Round(time.Second))
	}
	return ok, reason
}

// execProviderProbe runs the probe command. Tests replace this to simulate a
// stuck provider without talking to a real CLI.
var execProviderProbe = defaultExecProviderProbe

func defaultExecProviderProbe(ctx context.Context, command string, args []string, stdin string) (stdout, stderr string, err error, timedOut bool) {
	cmd := exec.CommandContext(ctx, command, args...)
	// A provider with a native single-prompt flag (Grok) gets no stdin at all;
	// os/exec then attaches no terminal and no interactive input surface.
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, runErr := cmd.Output()
	return string(out), errBuf.String(), runErr, ctx.Err() == context.DeadlineExceeded
}

// ReadPathProbes returns availability probes safe for resolve-lane / route
// advertising. Live generation probes are SKIPPED: a stuck provider CLI must
// not block fleet snapshot JSON (CHA-2451). CLI presence, surface readiness,
// and live quota remain. Launch admission must keep defaultProbes().
func ReadPathProbes() *Probes {
	p := &Probes{
		CLIPresent: func(cli string) bool {
			_, err := exec.LookPath(cli)
			return err == nil
		},
		// Explicit no-op probe: Launchable below does not call it. Kept non-nil
		// so callers that inspect ProviderProbe see a configured read-path skip.
		ProviderProbe: func(provider, model string) (bool, string) {
			return true, "read_path_probe_skipped"
		},
		Now: time.Now,
	}
	p.Launchable = func(provider, model string) (bool, string) {
		surface, ok := SurfaceFor(provider)
		if !ok {
			return false, fmt.Sprintf("unsupported routed provider %q", provider)
		}
		if provider == "kimi" && strings.TrimSpace(os.Getenv("HERD_KIMI_ENABLED")) != "1" {
			return false, "kimi provider is not configured"
		}
		if launchable, reason := ProbeSurface(surface); !launchable {
			return false, reason
		}
		// CHA-2451: do not invoke defaultProviderProbe / codex exec here.
		return true, "read_path_probe_skipped"
	}
	return p
}

// InstallReadPathProbes swaps a router's probes to the CHA-2451 read-path
// budget. Call after NewRouter for resolve-lane and herd route only.
func InstallReadPathProbes(r *SurfaceRouter) {
	if r == nil {
		return
	}
	r.Probes = ReadPathProbes()
}
