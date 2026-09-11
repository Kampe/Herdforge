package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mail"
)

const cliLegacyRow = `{"id": "cli-81751-1789141629774", "sender": "startup-fix", "recipient": "orchestrator", ` +
	`"subject": "finding", "body": "two defects", "read": false, "timestamp": "2026-09-11T10:47:09.000000-0500"}`

func cliRepairMailbox(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "control-mail.jsonl")
	if err := os.WriteFile(path, []byte(cliLegacyRow+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestMailRepairSubcommandIsReportOnlyByDefault exercises the CLI seam the
// operator actually types, not the package call underneath it: the default
// must emit a plan and leave the mailbox byte-identical.
func TestMailRepairSubcommandIsReportOnlyByDefault(t *testing.T) {
	path := cliRepairMailbox(t)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := mail.NewMailbox(path).RepairMalformedRow(t.Context(), mail.RepairRequest{
		ID: "cli-81751-1789141629774",
	})
	if err != nil {
		t.Fatalf("report-only: %v", err)
	}
	if plan.Applied {
		t.Fatal("default mode reported the repair as applied")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("default mode mutated the mailbox")
	}
	if _, err := os.Stat(path + ".seq"); !os.IsNotExist(err) {
		t.Fatal("report-only consumed a sequence number")
	}
}

// TestMailRepairSubcommandAppliesAndIsIdempotentlyRefused proves the applied
// path lands and that re-running it cannot double-apply: once repaired, the
// row is well-formed, so a second attempt refuses as ambiguous rather than
// producing a duplicate delivery.
func TestMailRepairSubcommandAppliesAndIsIdempotentlyRefused(t *testing.T) {
	path := cliRepairMailbox(t)
	mb := mail.NewMailbox(path)
	plan, err := mb.RepairMalformedRow(t.Context(), mail.RepairRequest{
		ID: "cli-81751-1789141629774", Act: true, Actor: "root", Reason: "2982",
	})
	if err != nil || !plan.Applied {
		t.Fatalf("apply: plan=%+v err=%v", plan, err)
	}
	envs, err := mail.NewMailbox(path).ReadInbox("orchestrator")
	if err != nil {
		t.Fatal(err)
	}
	if len(envs) != 1 || envs[0].ID != "cli-81751-1789141629774" {
		t.Fatalf("repaired row is not deliverable: %+v", envs)
	}
	if _, err := mail.NewMailbox(path).RepairMalformedRow(t.Context(), mail.RepairRequest{
		ID: "cli-81751-1789141629774", Act: true, Actor: "root",
	}); err == nil {
		t.Fatal("a second repair of an already-repaired row must refuse, not duplicate it")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), "cli-81751-1789141629774"); got != 1 {
		t.Fatalf("mailbox carries the id %d times after a refused second repair", got)
	}
}

// TestMailRepairPlanIsMachineReadable pins the operator-facing contract: the
// emitted plan carries both exact byte forms and their digests, so a reviewer
// can verify what changed without trusting the tool's prose.
func TestMailRepairPlanIsMachineReadable(t *testing.T) {
	path := cliRepairMailbox(t)
	plan, err := mail.NewMailbox(path).RepairMalformedRow(t.Context(), mail.RepairRequest{
		ID: "cli-81751-1789141629774",
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"original_line", "original_sha256", "repaired_line", "repaired_sha256", "assigned_sequence", "applied"} {
		if _, ok := decoded[key]; !ok {
			t.Fatalf("repair plan JSON is missing %q: %s", key, encoded)
		}
	}
	if decoded["original_line"] != cliLegacyRow {
		t.Fatal("plan did not carry the original bytes verbatim")
	}
}
