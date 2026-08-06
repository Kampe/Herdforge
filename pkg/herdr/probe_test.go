package herdr

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/toolpolicy"
)

// withStubOpencode puts a fake `opencode` on PATH whose behavior per model is
// scripted, so ProbeModel/ResolveHealthyModel are hermetic.
func withStubOpencode(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	stub := "#!/bin/sh\n" + script + "\n"
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestProbeModel_Healthy(t *testing.T) {
	withStubOpencode(t, `echo "PROBE_OK"`)
	r := ProbeModel(context.Background(), "litellm/lazer/deepseek-v4-flash")
	if !r.Available {
		t.Fatalf("healthy model must probe available: %+v", r)
	}
}

func TestProbeModel_Exhausted(t *testing.T) {
	withStubOpencode(t, `echo "No payment method. Add one at ..."; exit 1`)
	r := ProbeModel(context.Background(), "opencode/deepseek-v4-flash-free")
	if r.Available || r.Reason != "no payment method" {
		t.Fatalf("exhausted model must be caught: %+v", r)
	}
}

func TestProbeModel_QuotaSignal(t *testing.T) {
	withStubOpencode(t, `echo "error: quota exceeded for this key"; exit 1`)
	r := ProbeModel(context.Background(), "m")
	if r.Available || r.Reason != "quota" {
		t.Fatalf("quota signal must be caught: %+v", r)
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

func TestProbeProviderModel_CodexUsesExactCodexCLI(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "codex.args")
	opencodeMarker := filepath.Join(dir, "opencode.called")
	writeProbeCLI(t, dir, "codex", `printf '%s\n' "$@" > "`+argsPath+`"
out=
prev=
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then
    out=$a
  fi
  prev=$a
done
if [ -n "$out" ]; then
  printf '%s\n' "PROBE_OK" > "$out"
fi
echo '{"type":"turn.completed"}'`)
	writeProbeCLI(t, dir, "opencode", `echo called > "`+opencodeMarker+`"
echo quota exceeded
exit 91`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
	if !result.Available {
		t.Fatalf("exact Codex probe unavailable: %+v", result)
	}
	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	gotArgs := strings.Split(strings.TrimSuffix(string(got), "\n"), "\n")
	outputPath := ""
	for i, arg := range gotArgs {
		if arg == "--output-last-message" {
			if i+1 >= len(gotArgs) {
				t.Fatalf("missing path after --output-last-message in %q", gotArgs)
			}
			outputPath = gotArgs[i+1]
			if strings.TrimSpace(outputPath) == "" {
				t.Fatalf("blank path after --output-last-message in %q", gotArgs)
			}
			gotArgs[i+1] = "<probe-output>"
			break
		}
	}
	if outputPath == "" {
		t.Fatalf("Codex args missing --output-last-message: %q", gotArgs)
	}
	wantArgs := []string{
		"exec",
		"--model", "gpt-5.6-luna",
		"--sandbox", "read-only",
		"--skip-git-repo-check",
		"--json",
		"--ephemeral",
		"--output-last-message", "<probe-output>",
		"--config", "model_reasoning_effort=medium",
		"--config", toolpolicy.CodexDisableCodeReviewGraph,
		probePrompt,
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("Codex args = %q, want %q", gotArgs, wantArgs)
	}
	if _, err := os.Stat(outputPath); !os.IsNotExist(err) {
		t.Fatalf("probe output path still exists after return: %v", err)
	}
	if _, err := os.Stat(opencodeMarker); !os.IsNotExist(err) {
		t.Fatalf("Codex probe invoked OpenCode: %v", err)
	}
}

func TestProbeProviderModel_OpenCodeBackedUsesOpenCode(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "opencode.args")
	writeProbeCLI(t, dir, "opencode", `printf '%s\n' "$@" > "`+argsPath+`"
echo PROBE_OK`)
	writeProbeCLI(t, dir, "codex", `exit 91`)
	t.Setenv("PATH", dir)

	result := ProbeProviderModel(context.Background(), "lazer", "litellm/lazer/deepseek-v4-flash", "medium")
	if !result.Available {
		t.Fatalf("OpenCode-backed probe unavailable: %+v", result)
	}
	got, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := string(got)
	if !strings.Contains(args, "run\n--model\nlitellm/lazer/deepseek-v4-flash\n") {
		t.Fatalf("unexpected OpenCode args: %q", args)
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
			t.Fatalf("missing Codex executable did not fail closed: %+v", result)
		}
	})
	for _, tc := range []struct {
		name, script, reason string
	}{
		{name: "nonzero", script: "echo command failed; exit 2", reason: "probe failed:"},
		{name: "quota", script: "echo quota exceeded; exit 1", reason: "quota"},
		{name: "missing token", script: `echo '{"type":"turn.completed"}'`, reason: "no exact probe output"},
		{name: "non-exact token", script: `out=
prev=
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then
    out=$a
  fi
  prev=$a
done
if [ -n "$out" ]; then
  printf '%s\n' "NOT_PROBE_OK" > "$out"
fi
echo '{"type":"turn.completed"}'`, reason: "no exact probe output"},
		{name: "error after token", script: `out=
prev=
for a in "$@"; do
  if [ "$prev" = "--output-last-message" ]; then
    out=$a
  fi
  prev=$a
done
if [ -n "$out" ]; then
  printf '%s\n' "PROBE_OK" > "$out"
fi
echo '{"type":"turn.failed"}'`, reason: "no exact probe output"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeProbeCLI(t, dir, "codex", tc.script)
			t.Setenv("PATH", dir)
			result := ProbeProviderModel(context.Background(), "codex", "gpt-5.6-luna", "medium")
			if result.Available || !strings.HasPrefix(result.Reason, tc.reason) {
				t.Fatalf("%s did not fail closed: %+v", tc.name, result)
			}
		})
	}
}
