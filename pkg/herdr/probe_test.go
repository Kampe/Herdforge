package herdr

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// withStubOpencode installs a fake `opencode` binary on PATH that runs the
// given shell body. body is the shell script body (no shebang).
func withStubOpencode(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProbeModel_Healthy(t *testing.T) {
	withStubOpencode(t, `echo "PROBE_OK"`)
	r := ProbeModel(context.Background(), "litellm/lazer/deepseek-v4-flash")
	if !r.Available {
		t.Fatalf("expected available, got %+v", r)
	}
}

func TestProbeModel_Exhausted(t *testing.T) {
	withStubOpencode(t, `echo "No payment method. Add one at ..."; exit 1`)
	r := ProbeModel(context.Background(), "opencode/deepseek-v4-flash-free")
	if r.Available {
		t.Fatal("exhausted surface marked available")
	}
	if r.Reason != "no payment method" {
		t.Fatalf("reason = %q, want no payment method", r.Reason)
	}
}

func TestProbeModel_QuotaSignal(t *testing.T) {
	withStubOpencode(t, `echo "error: quota exceeded for this key"; exit 1`)
	r := ProbeModel(context.Background(), "m")
	if r.Available || r.Reason != "quota" {
		t.Fatalf("got %+v", r)
	}
}

func TestProbeModel_PeriodLimitSignals(t *testing.T) {
	for _, period := range []string{"weekly", "daily", "monthly"} {
		t.Run(period, func(t *testing.T) {
			withStubOpencode(t, `echo "You hit your `+period+` limit"; exit 1`)
			r := ProbeModel(context.Background(), "m")
			if r.Available || r.Reason != period+" limit" {
				t.Fatalf("period limit must be exhausted, got %+v", r)
			}
		})
	}
}

func TestResolveHealthyModel_FailsOver(t *testing.T) {
	// Primary emits an exhaustion signal; the fallback echoes PROBE_OK. The
	// stub distinguishes by the --model arg.
	withStubOpencode(t, `
for a in "$@"; do
  case "$a" in
    *free) echo "No payment method"; exit 1 ;;
    *lazer*) echo "PROBE_OK"; exit 0 ;;
  esac
done
echo "PROBE_OK"`)
	got, trail := ResolveHealthyModel(context.Background(),
		"opencode/deepseek-v4-flash-free",
		[]string{"litellm/lazer/deepseek-v4-flash"})
	if got != "litellm/lazer/deepseek-v4-flash" {
		t.Fatalf("must fail over to healthy fallback, got %q (trail %+v)", got, trail)
	}
	if len(trail) != 2 || trail[0].Available || !trail[1].Available {
		t.Fatalf("trail must show primary-exhausted then fallback-ok: %+v", trail)
	}
}

func TestResolveHealthyModel_AllExhausted(t *testing.T) {
	withStubOpencode(t, `echo "quota exceeded"; exit 1`)
	got, trail := ResolveHealthyModel(context.Background(), "a", []string{"b"})
	if got != "" {
		t.Fatalf("all-exhausted must return empty, got %q", got)
	}
	if len(trail) != 2 {
		t.Fatalf("trail must probe every candidate: %+v", trail)
	}
}

func TestResolveHealthyProviderModel_UsesConfiguredProviderForFallbacks(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "grok.args")
	writeProbeCLI(t, dir, "grok", `printf '%s\n' "$@" > "`+argsPath+`"
for a in "$@"; do
  case "$a" in
    primary-model) echo "primary failed" >&2; exit 2 ;;
  esac
done
echo PROBE_OK`)
	writeProbeCLI(t, dir, "opencode", `echo opencode-must-not-run >&2; exit 91`)
	t.Setenv("PATH", dir)

	got, trail := ResolveHealthyProviderModel(context.Background(), "grok", "primary-model", "medium", []string{"fallback-model"})
	if got != "fallback-model" {
		t.Fatalf("provider-aware resolver selected %q, want fallback-model (trail=%+v)", got, trail)
	}
	if len(trail) != 2 || trail[0].Available || !strings.Contains(trail[0].Reason, "primary failed") || !trail[1].Available {
		t.Fatalf("provider-aware trail lost primary/fallback results: %+v", trail)
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "fallback-model") {
		t.Fatalf("fallback was not probed through grok argv: %q", raw)
	}
}

func writeProbeCLI(t *testing.T, dir, name, script string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestProbeProviderModel_CodexUsesExactPiCLI(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "pi.args")
	codexMarker := filepath.Join(dir, "codex.called")
	opencodeMarker := filepath.Join(dir, "opencode.called")
	writeProbeCLI(t, dir, "pi", `printf '%s\n' "$@" > "`+argsPath+`"
echo PROBE_OK`)
	writeProbeCLI(t, dir, "codex", `echo called > "`+codexMarker+`"
exit 91`)
	writeProbeCLI(t, dir, "opencode", `echo called > "`+opencodeMarker+`"
exit 91`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
	if !result.Available {
		t.Fatalf("exact Pi probe unavailable: %+v", result)
	}
	if _, err := os.Stat(codexMarker); !os.IsNotExist(err) {
		t.Fatal("native codex CLI was invoked")
	}
	if _, err := os.Stat(opencodeMarker); !os.IsNotExist(err) {
		t.Fatal("opencode CLI was invoked")
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(raw)), "\n")
	wantArgs := []string{
		"--no-session",
		"--no-approve",
		"--no-context-files",
		"--no-extensions",
		"--no-skills",
		"--no-prompt-templates",
		"--no-tools",
		"--model", "openai-codex/gpt-5.6-luna",
		"--thinking", "medium",
		"-p", probePrompt,
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", gotArgs, wantArgs)
	}
}

func TestProbeProviderModel_UsesEachNativeHeadlessAdapter(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		effort   string
		command  string
	}{
		{provider: "codex", model: "gpt-5.6-luna", effort: "medium", command: "pi"},
		{provider: "grok", model: "grok-4.6", effort: "medium", command: "grok"},
		{provider: "agy", model: "gemini-2.5-pro", effort: "medium", command: "agy"},
		{provider: "opencode", model: "litellm/lazer/deepseek-v4-flash", command: "opencode"},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			dir := t.TempDir()
			marker := filepath.Join(dir, tc.command+".called")
			for _, command := range []string{"agy", "grok", "opencode", "pi"} {
				name := command
				if command == tc.command {
					writeProbeCLI(t, dir, name, `: > "`+marker+`"; printf '%s\n' PROBE_OK`)
					continue
				}
				writeProbeCLI(t, dir, name, `echo unexpected-`+name+` >&2; exit 91`)
			}
			t.Setenv("PATH", dir)

			result := ProbeProviderModel(context.Background(), tc.provider, tc.model, tc.effort)
			if !result.Available {
				t.Fatalf("native %s adapter failed: %+v", tc.provider, result)
			}
			if _, err := os.Stat(marker); err != nil {
				t.Fatalf("native %s adapter was not invoked: %v", tc.provider, err)
			}
		})
	}
}

func TestProbeProviderModel_OpenCodeBackedUsesOpenCode(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "opencode.args")
	writeProbeCLI(t, dir, "opencode", `printf '%s\n' "$@" > "`+argsPath+`"
echo PROBE_OK`)
	writeProbeCLI(t, dir, "codex", `exit 91`)
	writeProbeCLI(t, dir, "pi", `exit 91`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "lazer", "litellm/lazer/deepseek-v4-flash", "medium")
	if !result.Available {
		t.Fatalf("OpenCode-backed probe unavailable: %+v", result)
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSpace(string(raw)), "\n")
	wantArgs := []string{"run", "--model", "litellm/lazer/deepseek-v4-flash", probePrompt}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("argv mismatch\n got: %#v\nwant: %#v", gotArgs, wantArgs)
	}
}

func TestProbeProviderModel_OSCWrappedTokenPasses(t *testing.T) {
	dir := t.TempDir()
	writeProbeCLI(t, dir, "pi", `printf '\033]0;Herdforge: ready\007PROBE_OK\033]0;Herdforge: done\033\\'`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
	if !result.Available {
		t.Fatalf("OSC-wrapped exact token must pass: %+v", result)
	}
}

func TestProbeProviderModel_OSCWithoutTokenFails(t *testing.T) {
	dir := t.TempDir()
	writeProbeCLI(t, dir, "pi", `printf '\033]0;Herdforge: ready\007\033]0;Herdforge: done\033\\'`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
	if result.Available || result.Reason != "no exact probe output" {
		t.Fatalf("OSC output without the token must fail closed: %+v", result)
	}
}

func TestProbeProviderModel_SanitizesFailureDetail(t *testing.T) {
	dir := t.TempDir()
	writeProbeCLI(t, dir, "pi", `printf '\033]0;Herdforge: ready\007{"error":"first line
substantive multiline message"}\033]0;Herdforge: done\033\\' >&2; exit 2`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
	if result.Available || !strings.Contains(result.Reason, `"error":"first line`) || !strings.Contains(result.Reason, "substantive multiline message") || !strings.Contains(result.Reason, "exit status 2") {
		t.Fatalf("failure detail = %+v", result)
	}
	if strings.ContainsAny(result.Reason, "\x1b\a") {
		t.Fatalf("failure detail contains a raw control byte: %q", result.Reason)
	}
}

func TestProbeProviderModel_BoundsMultilineFailureDetail(t *testing.T) {
	dir := t.TempDir()
	writeProbeCLI(t, dir, "pi", `i=0; while [ "$i" -lt 5000 ]; do printf x >&2; i=$((i + 1)); done; exit 2`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
	if result.Available || !strings.Contains(result.Reason, probeDetailTruncation) {
		t.Fatalf("long failure was not explicitly bounded: %+v", result)
	}
	if len(result.Reason) > len("probe failed: ")+maxProbeFailureDetail {
		t.Fatalf("failure detail exceeded bound: %d", len(result.Reason))
	}
}

func TestBoundProbeFailureDetail_StatusWindowIsBounded(t *testing.T) {
	for _, statusLen := range []int{4066, 4067, 4070, 4071, 4072} {
		t.Run(fmt.Sprintf("status-%d", statusLen), func(t *testing.T) {
			status := strings.Repeat("s", statusLen)
			got := boundProbeFailureDetail(strings.Repeat("d", 5000), status)
			if len(got) > maxProbeFailureDetail {
				t.Fatalf("failure detail length = %d, want <= %d", len(got), maxProbeFailureDetail)
			}
			if !strings.Contains(got, strings.Repeat("s", 32)) {
				t.Fatalf("bounded failure detail lost status evidence: len=%d", len(got))
			}
		})
	}
}

func TestProbeProviderModel_FailsClosed(t *testing.T) {
	t.Run("unsupported provider", func(t *testing.T) {
		// FAC-578: this case used to assert that native CLAUDE was unsupported,
		// which is precisely the defect — a provider the router can route and
		// launch was unprobeable, so any preflight built on the probe refused a
		// valid route. A genuinely unlaunchable provider is the real subject.
		result := ProbeProviderModel(context.Background(), "definitely-not-a-provider", "m", "medium")
		if result.Available || !strings.Contains(result.Reason, "cannot probe what cannot be launched") {
			t.Fatalf("unsupported provider did not fail closed: %+v", result)
		}
	})
	t.Run("missing executable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
		if result.Available || !strings.HasPrefix(result.Reason, "probe failed:") {
			t.Fatalf("missing Pi executable did not fail closed: %+v", result)
		}
	})

	cases := []struct {
		name   string
		script string
		reason string
	}{
		{name: "nonzero", script: "echo command failed; exit 2", reason: "probe failed:"},
		{name: "quota", script: "echo quota exceeded; exit 1", reason: "quota"},
		{name: "missing token", script: `exit 0`, reason: "no exact probe output"},
		{name: "nonexact token", script: `echo NOT_PROBE_OK`, reason: "no exact probe output"},
		{name: "extra token", script: `printf 'PROBE_OK\nextra\n'`, reason: "no exact probe output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProbeCLI(t, dir, "pi", tc.script)
			t.Setenv("PATH", dir)
			result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
			if result.Available || !strings.HasPrefix(result.Reason, tc.reason) {
				t.Fatalf("%s did not fail closed: %+v", tc.name, result)
			}
		})
	}
}
