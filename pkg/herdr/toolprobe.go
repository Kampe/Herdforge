package herdr

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// FAC-96: does this surface actually EXECUTE a tool, or just talk about it?
//
// The most dangerous failure mode in the fleet is not a crash — it is a
// confident, well-formatted, entirely fictional result. A model can emit
// {"name":"write",...} as TEXT without executing anything: it reasons, it
// knows the shape of the call it wants, but nothing runs. An agent on such a
// surface cannot write a verdict, run a test, or commit — it only produces
// prose that LOOKS like work (observed live 2026-08-02: glm/qwen emitted tool
// JSON as text; only some lazer models actually executed).
//
// ToolProbe gives the model a task that REQUIRES a real tool call — write a
// sentinel file — and checks the artifact appeared. Exit/return true only
// when the file was actually created, so doctor-models can refuse a
// surface that cannot execute tools BEFORE the fleet dispatches real work to
// it.

// ToolProbeResult reports whether a model executed a real tool.
type ToolProbeResult struct {
	Model    string
	Executes bool
	Reason   string // "" when it executes; else why it failed
}

// ToolProbe runs the model on a write-a-sentinel task in a scratch dir and
// verifies the file materialized. A model that only describes the write
// (never runs it) fails — that is the whole point.
func ToolProbe(ctx context.Context, model string) ToolProbeResult {
	dir, err := os.MkdirTemp("", "herd-toolprobe-")
	if err != nil {
		return ToolProbeResult{Model: model, Executes: false, Reason: "scratch dir: " + err.Error()}
	}
	defer os.RemoveAll(dir)

	sentinel := filepath.Join(dir, "PROBE_OK.txt")
	prompt := "Use your write/file tool to create the file " + sentinel +
		" containing exactly the text EXECUTED. Do not print the file content — actually create the file."

	pctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, "opencode", "run", "--model", model, prompt)
	out, _ := cmd.CombinedOutput()

	// The verdict is the ARTIFACT, never the model's claim: check the file.
	data, err := os.ReadFile(sentinel)
	if err != nil {
		lower := strings.ToLower(string(out))
		for _, sig := range exhaustionSignals {
			if strings.Contains(lower, sig) {
				return ToolProbeResult{Model: model, Executes: false, Reason: "surface unavailable: " + sig}
			}
		}
		return ToolProbeResult{Model: model, Executes: false,
			Reason: "no file created — model described the write but did not execute the tool"}
	}
	if !strings.Contains(string(data), "EXECUTED") {
		return ToolProbeResult{Model: model, Executes: false, Reason: "file created but wrong content"}
	}
	return ToolProbeResult{Model: model, Executes: true}
}
