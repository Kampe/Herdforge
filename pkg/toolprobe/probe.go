package toolprobe

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Runner executes one artifact probe for an identity. Tests inject fakes;
// production uses DefaultRunner.
type Runner interface {
	Probe(ctx context.Context, id Identity) Receipt
}

// ExecRunner runs provider-specific recipes that require a real tool write.
// Command is injectable for tests (default: exec.CommandContext).
type ExecRunner struct {
	// Command builds a cancellable command. When nil, uses exec.CommandContext.
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
	// Now is injectable clock; defaults to time.Now.
	Now func() time.Time
	// TTL overrides DefaultTTL when > 0.
	TTL time.Duration
	// Timeout bounds one probe attempt.
	Timeout time.Duration
}

func (r *ExecRunner) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *ExecRunner) ttl() time.Duration {
	if r != nil && r.TTL > 0 {
		return r.TTL
	}
	return DefaultTTL
}

func (r *ExecRunner) timeout() time.Duration {
	if r != nil && r.Timeout > 0 {
		return r.Timeout
	}
	return 90 * time.Second
}

func (r *ExecRunner) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	if r != nil && r.Command != nil {
		return r.Command(ctx, name, args...)
	}
	return exec.CommandContext(ctx, name, args...)
}

// Probe runs the artifact-write recipe for id and returns a signed receipt.
func (r *ExecRunner) Probe(ctx context.Context, id Identity) Receipt {
	if err := id.Valid(); err != nil {
		return mustReceipt(id, StatusUNKNOWN, err.Error(), "", r.now(), r.ttl())
	}
	if id.Recipe != RecipeArtifactWrite {
		return mustReceipt(id, StatusTOOLING, "unsupported probe recipe "+id.Recipe, "", r.now(), r.ttl())
	}

	dir, err := os.MkdirTemp("", "herd-toolprobe-")
	if err != nil {
		return mustReceipt(id, StatusTOOLING, "scratch dir: "+err.Error(), "", r.now(), r.ttl())
	}
	defer os.RemoveAll(dir)

	sentinel := filepath.Join(dir, "PROBE_OK.txt")
	prompt := "Use your write/file tool to create the file " + sentinel +
		" containing exactly the text EXECUTED. Do not print the file content — actually create the file."

	pctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()

	name, args, err := recipeCommand(id, prompt)
	if err != nil {
		return mustReceipt(id, StatusTOOLING, err.Error(), "", r.now(), r.ttl())
	}
	cmd := r.command(pctx, name, args...)
	out, runErr := cmd.CombinedOutput()
	combined := string(out)

	data, readErr := os.ReadFile(sentinel)
	if readErr == nil && strings.Contains(string(data), "EXECUTED") {
		sum := sha256.Sum256(data)
		return mustReceipt(id, StatusPASS, "", "sha256:"+hex.EncodeToString(sum[:]), r.now(), r.ttl())
	}

	status, reason := classifyFailure(combined, runErr, pctx.Err(), readErr)
	return mustReceipt(id, status, reason, "", r.now(), r.ttl())
}

func recipeCommand(id Identity, prompt string) (string, []string, error) {
	provider := norm(id.Provider)
	harness := norm(id.Harness)
	model := strings.TrimSpace(id.Model)
	if model == "" {
		return "", nil, fmt.Errorf("probe model is required")
	}
	// Pi harness (logical codex/claude/grok routes): enable tools and run once.
	if harness == "pi" || provider == "codex" || provider == "claude" || provider == "grok" {
		// Prefer the real harness binary (pi). Model is the logical model id.
		return "pi", []string{
			"--no-session",
			"--no-approve",
			"--no-context-files",
			"--no-extensions",
			"--no-skills",
			"--no-prompt-templates",
			"--model", model,
			"-p", prompt,
		}, nil
	}
	// OpenCode-backed surfaces (opencode/ollama/lazer).
	if provider == "opencode" || provider == "ollama" || provider == "lazer" || harness == "opencode" {
		return "opencode", []string{"run", "--model", model, prompt}, nil
	}
	return "", nil, fmt.Errorf("no artifact tool-probe recipe for provider %q harness %q", id.Provider, id.Harness)
}

func classifyFailure(combined string, runErr, ctxErr, readErr error) (Status, string) {
	lower := strings.ToLower(combined)
	for _, sig := range []struct {
		status Status
		need   string
	}{
		{StatusAUTH, "unauthorized"},
		{StatusAUTH, "forbidden"},
		{StatusAUTH, "authentication"},
		{StatusAUTH, "not logged in"},
		{StatusQUOTA, "no payment method"},
		{StatusQUOTA, "insufficient"},
		{StatusQUOTA, "quota"},
		{StatusQUOTA, "out of credit"},
		{StatusQUOTA, "payment required"},
		{StatusQUOTA, "billing"},
		{StatusQUOTA, "usage limit"},
		{StatusRateLimit, "rate limit"},
		{StatusRateLimit, "rate_limit"},
		{StatusRateLimit, "429"},
		{StatusRateLimit, "exhausted"},
	} {
		if strings.Contains(lower, sig.need) {
			return sig.status, sig.need
		}
	}
	if ctxErr == context.DeadlineExceeded {
		return StatusUNKNOWN, "probe timeout"
	}
	// Missing binary / bad recipe → TOOLING, not model INCAPABLE.
	if runErr != nil {
		msg := runErr.Error()
		if strings.Contains(msg, "executable file not found") ||
			strings.Contains(msg, "no such file") ||
			strings.Contains(lower, "command not found") {
			return StatusTOOLING, "probe harness unavailable: " + firstLine(combined+"\n"+msg)
		}
	}
	if readErr != nil {
		// File never appeared: model described the write or ignored tools.
		return StatusINCAPABLE, "no file created — model described the write but did not execute the tool"
	}
	return StatusINCAPABLE, "file created but wrong content"
}

func firstLine(s string) string {
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			return t
		}
	}
	return strings.TrimSpace(s)
}

func mustReceipt(id Identity, status Status, reason, proof string, now time.Time, ttl time.Duration) Receipt {
	r, err := NewReceipt(id, status, reason, proof, now, ttl)
	if err != nil {
		// Identity was validated at entry; this only fires on programmer error.
		return Receipt{SchemaVersion: SchemaVersion, Identity: id, Status: StatusUNKNOWN, Reason: err.Error(), ProbedAt: now, ExpiresAt: now.Add(ttl)}
	}
	return r
}

// Ensure returns a fresh PASS receipt for id, using cache then runner.
// Non-PASS cached results are returned as-is (caller decides failover).
// Missing cache or expired entry re-probes.
func Ensure(ctx context.Context, id Identity, cache Cache, runner Runner, now time.Time) (Receipt, error) {
	if err := id.Valid(); err != nil {
		return Receipt{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if r, ok := LookupFresh(cache, id, now); ok {
		return r, nil
	}
	if runner == nil {
		return Receipt{}, fmt.Errorf("toolprobe: runner required when cache misses")
	}
	r := runner.Probe(ctx, id)
	if cache != nil {
		_ = cache.Put(r)
	}
	return r, nil
}

// StaticRunner always returns the configured receipt (tests).
type StaticRunner struct {
	Receipt Receipt
}

func (s StaticRunner) Probe(context.Context, Identity) Receipt { return s.Receipt }

// FuncRunner adapts a function to Runner.
type FuncRunner func(context.Context, Identity) Receipt

func (f FuncRunner) Probe(ctx context.Context, id Identity) Receipt { return f(ctx, id) }
