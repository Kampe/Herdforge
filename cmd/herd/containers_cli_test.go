package main

import (
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestHerdContainersCLIJSONFreshStore(t *testing.T) {
	binary := buildHerd(t)
	dbPath := filepath.Join(t.TempDir(), "receipts.db")

	cmd := exec.Command(binary, "containers", "--json", "--db", dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd containers --json failed: %v\n%s", err, out)
	}

	var report struct {
		OwnedActive          []any  `json:"owned_active"`
		OwnedAwaitingCleanup []any  `json:"owned_awaiting_cleanup"`
		Removed              []any  `json:"removed"`
		Quarantined          []any  `json:"quarantined"`
		AuditError           string `json:"audit_error"`
	}
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	if len(report.OwnedActive) != 0 || len(report.OwnedAwaitingCleanup) != 0 || len(report.Removed) != 0 || len(report.Quarantined) != 0 {
		t.Fatalf("fresh store should report no receipts, got %+v", report)
	}
}

func TestHerdContainersCLIHumanFreshStore(t *testing.T) {
	binary := buildHerd(t)
	dbPath := filepath.Join(t.TempDir(), "receipts.db")

	cmd := exec.Command(binary, "containers", "--db", dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd containers failed: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"owned active:", "owned awaiting cleanup:", "removed (absence proved):", "quarantined:"} {
		if !strings.Contains(s, want) {
			t.Errorf("expected %q in output, got:\n%s", want, s)
		}
	}
}

func TestHerdContainersCLIHelp(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "containers", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("herd containers --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-containers") {
		t.Errorf("expected herd-containers header, got:\n%s", out)
	}
}

func TestHerdContainersCLIUnknownArg(t *testing.T) {
	binary := buildHerd(t)
	dbPath := filepath.Join(t.TempDir(), "receipts.db")
	cmd := exec.Command(binary, "containers", "--db", dbPath, "--bogus")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
}

// reconcileJSONReport mirrors containerlifecycle.ReconcileReport's JSON
// shape exactly — used to assert dry-run and --apply share one schema.
type reconcileJSONReport struct {
	DryRun      bool     `json:"dry_run"`
	Reclaimed   []string `json:"reclaimed"`
	Quarantined []string `json:"quarantined"`
	Skipped     []string `json:"skipped"`
}

func TestHerdContainersReconcileDryRunEmptyStore(t *testing.T) {
	binary := buildHerd(t)
	dbPath := filepath.Join(t.TempDir(), "receipts.db")

	cmd := exec.Command(binary, "containers", "reconcile", "--json", "--db", dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd containers reconcile failed: %v\n%s", err, out)
	}
	var report reconcileJSONReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	if !report.DryRun {
		t.Fatal("default invocation must be a dry run")
	}
	if len(report.Reclaimed) != 0 || len(report.Skipped) != 0 || len(report.Quarantined) != 0 {
		t.Fatalf("fresh store should have nothing to reconcile, got %+v", report)
	}
}

func TestHerdContainersReconcileApplyEmptyStoreNeverTouchesDocker(t *testing.T) {
	binary := buildHerd(t)
	dbPath := filepath.Join(t.TempDir(), "receipts.db")

	// An empty receipt store means Reconcile has zero candidates, so this
	// must succeed even on a host without docker installed — it never
	// needs to shell out.
	cmd := exec.Command(binary, "containers", "reconcile", "--apply", "--json", "--db", dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("herd containers reconcile --apply failed: %v\n%s", err, out)
	}
	var report reconcileJSONReport
	if err := json.Unmarshal(out, &report); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	if report.DryRun {
		t.Fatal("--apply must not report dry_run=true")
	}
	if len(report.Reclaimed) != 0 || len(report.Quarantined) != 0 || len(report.Skipped) != 0 {
		t.Fatalf("fresh store should reconcile nothing, got %+v", report)
	}
}

// TestHerdContainersReconcileDryRunAndApplyShareOneJSONSchema guards
// against the class of bug where dry-run and --apply silently drift to
// different, differently-cased JSON shapes (dry_run/would_reclaim/
// still_live vs untagged PascalCase Reclaimed/Quarantined/Skipped) —
// both must decode into the exact same struct.
func TestHerdContainersReconcileDryRunAndApplyShareOneJSONSchema(t *testing.T) {
	binary := buildHerd(t)

	dryRunDB := filepath.Join(t.TempDir(), "receipts.db")
	dryOut, err := exec.Command(binary, "containers", "reconcile", "--json", "--db", dryRunDB).CombinedOutput()
	if err != nil {
		t.Fatalf("dry run failed: %v\n%s", err, dryOut)
	}
	var dryReport map[string]any
	if err := json.Unmarshal(dryOut, &dryReport); err != nil {
		t.Fatalf("dry run JSON: %v\n%s", err, dryOut)
	}

	applyDB := filepath.Join(t.TempDir(), "receipts.db")
	applyOut, err := exec.Command(binary, "containers", "reconcile", "--apply", "--json", "--db", applyDB).CombinedOutput()
	if err != nil {
		t.Fatalf("--apply failed: %v\n%s", err, applyOut)
	}
	var applyReport map[string]any
	if err := json.Unmarshal(applyOut, &applyReport); err != nil {
		t.Fatalf("--apply JSON: %v\n%s", err, applyOut)
	}

	wantKeys := []string{"dry_run", "reclaimed", "quarantined", "skipped"}
	for _, k := range wantKeys {
		if _, ok := dryReport[k]; !ok {
			t.Errorf("dry run report missing key %q: %v", k, dryReport)
		}
		if _, ok := applyReport[k]; !ok {
			t.Errorf("--apply report missing key %q: %v", k, applyReport)
		}
	}
	if len(dryReport) != len(wantKeys) {
		t.Errorf("dry run report has extra/unexpected keys: %v", dryReport)
	}
	if len(applyReport) != len(wantKeys) {
		t.Errorf("--apply report has extra/unexpected keys: %v", applyReport)
	}
}

func TestHerdContainersReconcileHelp(t *testing.T) {
	binary := buildHerd(t)
	out, err := exec.Command(binary, "containers", "reconcile", "--help").CombinedOutput()
	if err != nil {
		t.Fatalf("herd containers reconcile --help failed: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-containers reconcile") {
		t.Errorf("expected herd-containers reconcile header, got:\n%s", out)
	}
}

func TestHerdContainersReconcileUnknownArg(t *testing.T) {
	binary := buildHerd(t)
	dbPath := filepath.Join(t.TempDir(), "receipts.db")
	cmd := exec.Command(binary, "containers", "reconcile", "--db", dbPath, "--bogus")
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected non-zero exit for unknown arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
}
