package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heldBackupFixture builds a bare .herd/runtime-previous directory under a
// temp root. It deliberately does NOT build a runtime binary: every candidate
// in these tests is HELD, and the installed-target recheck only runs
// immediately before a removal, so a held-only pass never needs one.
func heldBackupFixture(t *testing.T) (root, backupDir, journalPath string, installer HerdRuntimeInstaller) {
	t.Helper()
	root = t.TempDir()
	backupDir = filepath.Join(root, ".herd", "runtime-previous")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installer = HerdRuntimeInstaller{Root: root}
	_, journalPath = installer.retentionPaths()
	return root, backupDir, journalPath, installer
}

// writeBackup drops one regular, single-link, current-owner file into the
// backup directory so it reaches the allowlist decision rather than being
// held on ownership or type.
func writeBackup(t *testing.T, backupDir, name, body string) string {
	t.Helper()
	path := filepath.Join(backupDir, name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// displacedBindingFor records the exact positive-allowlist identity for a
// backup file: repository-relative path plus digest, size and mod time.
func displacedBindingFor(t *testing.T, root, path, revision string) RuntimeRetentionBinding {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := runtimeFileDigest(path)
	if err != nil {
		t.Fatal(err)
	}
	return RuntimeRetentionBinding{
		RuntimeBinding:  RuntimeBinding{Revision: revision, Digest: digest, Executable: "bin/herd"},
		Path:            runtimeRetentionRelativePath(root, path),
		Size:            info.Size(),
		ModTimeUnixNano: info.ModTime().UnixNano(),
	}
}

func presentOwner(context.Context, string) (RuntimeOwnerStatus, error) {
	return RuntimeOwnerPresent, nil
}

// journalCompleteReason returns the reason recorded on the terminal
// "complete" journal event, which is the durable diagnostic an operator
// reads back after a partial pass.
func journalCompleteReason(t *testing.T, journalPath string) string {
	t.Helper()
	body, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatalf("journal: %v", err)
	}
	reason := ""
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var event RuntimeRetentionEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		if event.Event == "complete" {
			reason = event.Reason
		}
	}
	return reason
}

// A held candidate always has a known cause: the pass recorded one into the
// journal for every hold. Reporting "held-unknown-candidate" instead of that
// cause is the defect -- it turns a precise, actionable refusal into a
// permanent mystery that reads like a bug in the retention pass itself.
func TestRetireRuntimeBackupsReportsPreciseHeldReasons(t *testing.T) {
	root, backupDir, journalPath, installer := heldBackupFixture(t)

	orphan := writeBackup(t, backupDir, "aaa-pre-manifest-orphan", "pre-manifest backup body")
	displaced := writeBackup(t, backupDir, "bbb-displaced-live-owner", "displaced backup body")

	manifest := RuntimeRetentionManifest{
		Version: runtimeRetentionManifestVersion,
		Current: RuntimeRetentionBinding{
			RuntimeBinding: RuntimeBinding{Revision: strings.Repeat("c", 40), Digest: "sha256:current", Executable: "bin/herd"},
			Path:           "bin/herd",
		},
		Displaced: []RuntimeRetentionBinding{displacedBindingFor(t, root, displaced, strings.Repeat("d", 40))},
	}

	report, err := installer.retireRuntimeBackupsWith(context.Background(), manifest, journalPath, 16, 1<<30, presentOwner)
	if err != nil {
		t.Fatalf("retire: unexpected hard error: %v", err)
	}

	if report.Removed != 0 || report.Held != 2 || report.Protected != 0 || report.Errors != 0 {
		t.Fatalf("both candidates must be held and none removed: removed=%d held=%d protected=%d errors=%d",
			report.Removed, report.Held, report.Protected, report.Errors)
	}

	// The negative assertion: the generic label must never stand in for a
	// cause the pass already knows.
	if report.Reason == "held-unknown-candidate" {
		t.Fatalf("held reason collapsed to the generic label instead of the recorded causes: %q", report.Reason)
	}

	const want = "held:live-owner-unknown-or-present=1,unbound-candidate-not-in-retention-manifest=1"
	if report.Reason != want {
		t.Fatalf("report reason must name every held cause with its count in ascending order:\n got  %q\n want %q", report.Reason, want)
	}

	if got := report.HeldReasons["unbound-candidate-not-in-retention-manifest"]; got != 1 {
		t.Fatalf("missing pre-manifest authority must be counted by its own reason: got %d", got)
	}
	if got := report.HeldReasons["live-owner-unknown-or-present"]; got != 1 {
		t.Fatalf("live owner hold must be counted by its own reason: got %d", got)
	}
	if len(report.HeldReasons) != 2 {
		t.Fatalf("held reason accounting must contain exactly the two observed causes: %v", report.HeldReasons)
	}

	// The durable diagnostic has to carry the precise cause too, or an
	// operator reading the journal back still sees a mystery.
	if got := journalCompleteReason(t, journalPath); got != want {
		t.Fatalf("journal complete event reason:\n got  %q\n want %q", got, want)
	}

	// Missing authority stays HELD. Precise reporting must not become a
	// licence to delete an unbound backup.
	for _, path := range []string{orphan, displaced} {
		if _, statErr := os.Lstat(path); statErr != nil {
			t.Fatalf("held candidate must survive the pass: %s: %v", path, statErr)
		}
	}
}

// End-to-end shape of the reported production defect: a retained-unresolved
// manifest is retried on the next install, the retry legitimately holds a
// pre-manifest backup it has no authority to remove, and the manifest is
// then stamped with a reason an operator can act on -- not the generic
// label, which reads as "the retention pass is broken" and cost three
// separate investigations.
func TestResolveRetainedMaintenanceStampsPreciseReasonNotGenericLabel(t *testing.T) {
	_, backupDir, _, installer := heldBackupFixture(t)
	installer.retention = &RuntimeRetentionOptions{LiveOwner: absentOwner}
	manifestPath, _ := installer.retentionPaths()

	orphan := writeBackup(t, backupDir, "pre-manifest-orphan", "pre-manifest backup body")

	retained := RuntimeRetentionManifest{
		Version: runtimeRetentionManifestVersion,
		Current: RuntimeRetentionBinding{
			RuntimeBinding: RuntimeBinding{Revision: strings.Repeat("c", 40), Digest: "sha256:current", Executable: "bin/herd"},
			Path:           "bin/herd",
		},
		MaintenanceUnresolved: true,
		MaintenanceReason:     "held-unknown-candidate",
	}
	if err := writeRuntimeRetentionManifest(manifestPath, retained); err != nil {
		t.Fatal(err)
	}

	binding := &RuntimeBinding{Revision: strings.Repeat("c", 40), Digest: "sha256:current", Executable: "bin/herd"}
	got, err := installer.ResolveRetainedMaintenance(context.Background(), binding)
	if err == nil {
		t.Fatal("an unresolvable held candidate must keep surfacing the hard partial error")
	}
	if got == nil {
		t.Fatal("the installed binding must be preserved across a partial maintenance result")
	}

	const want = "held:unbound-candidate-not-in-retention-manifest=1"
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("partial maintenance error must carry the precise cause:\n got  %v\n want substring %q", err, want)
	}

	raw := map[string]any{}
	body, readErr := os.ReadFile(manifestPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if flag, _ := raw["maintenance_unresolved"].(bool); !flag {
		t.Fatal("an unresolved hold must keep the maintenance flag set")
	}
	reason, _ := raw["maintenance_reason"].(string)
	if reason == "held-unknown-candidate" {
		t.Fatalf("manifest still carries the generic label after a retry that knew the cause: %q", reason)
	}
	if reason != want {
		t.Fatalf("manifest maintenance_reason:\n got  %q\n want %q", reason, want)
	}

	if _, statErr := os.Lstat(orphan); statErr != nil {
		t.Fatalf("a backup with no removal authority must stay held, never deleted: %v", statErr)
	}
}

// Repeated identical causes must aggregate with a count rather than
// collapsing to one anonymous label, and the rendering must be deterministic
// so the manifest does not churn between otherwise identical passes.
func TestRetireRuntimeBackupsAggregatesRepeatedHeldReasonDeterministically(t *testing.T) {
	_, backupDir, journalPath, installer := heldBackupFixture(t)

	for _, name := range []string{"orphan-one", "orphan-two", "orphan-three"} {
		writeBackup(t, backupDir, name, "body "+name)
	}

	manifest := RuntimeRetentionManifest{
		Version: runtimeRetentionManifestVersion,
		Current: RuntimeRetentionBinding{
			RuntimeBinding: RuntimeBinding{Revision: strings.Repeat("c", 40), Digest: "sha256:current", Executable: "bin/herd"},
			Path:           "bin/herd",
		},
	}

	const want = "held:unbound-candidate-not-in-retention-manifest=3"
	first, err := installer.retireRuntimeBackupsWith(context.Background(), manifest, journalPath, 16, 1<<30, absentOwner)
	if err != nil {
		t.Fatalf("retire: unexpected hard error: %v", err)
	}
	if first.Held != 3 {
		t.Fatalf("every unbound candidate must be held: held=%d", first.Held)
	}
	if first.Reason == "held-unknown-candidate" {
		t.Fatalf("three known holds still collapsed to the generic label: %q", first.Reason)
	}
	if first.Reason != want {
		t.Fatalf("repeated cause must aggregate with a count:\n got  %q\n want %q", first.Reason, want)
	}

	second, err := installer.retireRuntimeBackupsWith(context.Background(), manifest, journalPath, 16, 1<<30, absentOwner)
	if err != nil {
		t.Fatalf("retire rerun: unexpected hard error: %v", err)
	}
	if second.Reason != first.Reason {
		t.Fatalf("held reason rendering must be deterministic across passes: first=%q second=%q", first.Reason, second.Reason)
	}
}
