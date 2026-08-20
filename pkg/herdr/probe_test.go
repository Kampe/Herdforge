package herdr

import (
	"context"
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

func TestProbeProviderModel_FailsClosed(t *testing.T) {
	t.Run("unsupported provider", func(t *testing.T) {
		result := ProbeProviderModel(context.Background(), "claude", "claude-sonnet-5", "medium")
		if result.Available || !strings.Contains(result.Reason, "unsupported probe provider") {
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
