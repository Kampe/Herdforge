package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/launch"
)

// TestDispatchTicketDecisionAttemptIdentityDistinguishesIndependentAttempts
// drives the real dispatchTicketDecision public admission path. The fixture
// deliberately reaches the real hook-policy refusal before Dispatch can touch
// a provider, claim, worktree, or agent. Each public call mints one attempt;
// the durable refusal receipts therefore prove that identical dispatches are
// distinct attempts rather than one process-wide/PID-deduplicated event.
func TestDispatchTicketDecisionAttemptIdentityDistinguishesIndependentAttempts(t *testing.T) {
	root, _, lane := standingClaudeAttemptFixture(t)
	if err := os.WriteFile(filepath.Join(root, ".herd", "herd.yaml"), []byte(`version: "1"
project:
  name: dispatch-attempt-fixture
task_provider:
  type: memory
  project_id: fixture
lanes:
  - name: chain-indexer
    role: worker
    agent_kind: claude
    harness: claude
    provider: claude
    model: claude-sonnet-5
    effort: medium
    task_shape: implementation
    standing: true
    worktree: .
    prompt: .herd/prompts/worker.md
    authority: write
    capabilities: [git-write]
`), 0o600); err != nil {
		t.Fatal(err)
	}

	for attempt := 1; attempt <= 2; attempt++ {
		if _, _, err := dispatchTicketDecision(context.Background(), dispatchRequest{
			TicketRef:    "FAC-1452",
			LaneName:     lane.Name,
			LaneExplicit: true,
		}, io.Discard); err == nil {
			t.Fatalf("dispatch admission attempt %d unexpectedly succeeded", attempt)
		} else {
			t.Logf("dispatch admission attempt %d: %v", attempt, err)
		}
	}

	data, err := os.ReadFile(filepath.Join(root, ".herd", "launch-receipts.jsonl"))
	if err != nil {
		t.Fatalf("read dispatch refusal receipts: %v", err)
	}
	var receipts []launch.Receipt
	for _, line := range splitNonEmpty(string(data)) {
		var receipt launch.Receipt
		if err := json.Unmarshal([]byte(line), &receipt); err != nil {
			t.Fatalf("decode dispatch refusal receipt: %v", err)
		}
		if receipt.HookCode == "hook.policy_set_missing" {
			receipts = append(receipts, receipt)
		}
	}
	if len(receipts) != 2 {
		t.Fatalf("dispatch public path recorded %d policy-set refusals, want 2: %+v", len(receipts), receipts)
	}
	if receipts[0].ReceiptKey == receipts[1].ReceiptKey {
		t.Fatalf("independent dispatch attempts collapsed onto one receipt key: %q", receipts[0].ReceiptKey)
	}
}

func splitNonEmpty(s string) []string {
	var lines []string
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
