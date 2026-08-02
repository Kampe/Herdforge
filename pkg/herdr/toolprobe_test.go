package herdr

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// stubOpencodeTool installs a fake `opencode` that either WRITES the sentinel
// (executes) or only prints tool JSON as text (does not execute), per mode.
func stubOpencodeTool(t *testing.T, mode string) {
	t.Helper()
	dir := t.TempDir()
	var script string
	switch mode {
	case "executes":
		// Parse the sentinel path out of the prompt (last .txt token) and write it.
		script = `#!/bin/sh
for a in "$@"; do
  case "$a" in
    */PROBE_OK.txt*) f=$(printf '%s' "$a" | grep -oE '/[^ ]*PROBE_OK.txt'); printf 'EXECUTED' > "$f"; echo "created $f" ;;
  esac
done`
	case "talks":
		script = `#!/bin/sh
echo '{"name":"write","path":"PROBE_OK.txt","content":"EXECUTED"}'`
	case "exhausted":
		script = `#!/bin/sh
echo "No payment method"; exit 1`
	}
	if err := os.WriteFile(filepath.Join(dir, "opencode"), []byte(script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestToolProbe_ExecutingModelPasses(t *testing.T) {
	stubOpencodeTool(t, "executes")
	r := ToolProbe(context.Background(), "litellm/lazer/gpt-5.6-sol")
	if !r.Executes {
		t.Fatalf("executing model must pass: %+v", r)
	}
}

func TestToolProbe_TalkingModelFails(t *testing.T) {
	stubOpencodeTool(t, "talks")
	r := ToolProbe(context.Background(), "litellm/ollama/glm-5.2:cloud")
	if r.Executes {
		t.Fatal("a model that only emits tool JSON as text must FAIL the probe")
	}
	if r.Reason == "" {
		t.Fatal("failure must carry a reason")
	}
}

func TestToolProbe_ExhaustedSurfaceFails(t *testing.T) {
	stubOpencodeTool(t, "exhausted")
	r := ToolProbe(context.Background(), "opencode/deepseek-v4-flash-free")
	if r.Executes {
		t.Fatal("exhausted surface must fail")
	}
}
