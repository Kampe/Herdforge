package harvest

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// heldOwner reports every candidate as held by a live owner, mimicking the
// review fixture where a displaced authenticated backup still had a live
// owner.
func heldOwner(context.Context, string) (RuntimeOwnerStatus, error) {
	return RuntimeOwnerPresent, nil
}

// absentOwner reports every candidate as owner-absent.
func absentOwner(context.Context, string) (RuntimeOwnerStatus, error) {
	return RuntimeOwnerAbsent, nil
}

// runtimeNextRevision commits and builds a new runtime revision in the
// fixture source worktree and returns an installer bound to it.
func runtimeNextRevision(t *testing.T, f runtimeFixture, marker string) HerdRuntimeInstaller {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.source, "main.go"), []byte("package main\n\nimport \"time\"\n\nfunc main() { time.Sleep("+marker+" * time.Second) }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, f.source, "add", "main.go")
	runtimeGit(t, f.source, "commit", "-m", "runtime "+marker)
	rev := runtimeGit(t, f.source, "rev-parse", "HEAD")
	runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", rev)
	runtimeBuild(t, f.source)
	return HerdRuntimeInstaller{Root: f.root, Source: f.source, Revision: rev, retention: f.installer.retention}
}

// retentionManifestFor loads the on-disk retention manifest for typed
// identity assertions (Displaced/Previous/Current pre-exist on the
// admitted candidate).
func retentionManifestFor(t *testing.T, f runtimeFixture) RuntimeRetentionManifest {
	t.Helper()
	manifestPath, _ := f.installer.retentionPaths()
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("retention manifest: %v", err)
	}
	var manifest RuntimeRetentionManifest
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("retention manifest decode: %v", err)
	}
	return manifest
}

// retentionManifestRawFor loads the raw on-disk retention manifest for
// flag-level assertions that must also compile against the admitted
// candidate (which lacks the unresolved-state fields).
func retentionManifestRawFor(t *testing.T, f runtimeFixture) map[string]any {
	t.Helper()
	manifestPath, _ := f.installer.retentionPaths()
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("retention manifest: %v", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(body, &manifest); err != nil {
		t.Fatalf("retention manifest decode: %v", err)
	}
	return manifest
}

func manifestFlagTrue(raw map[string]any) bool {
	flag, ok := raw["maintenance_unresolved"].(bool)
	return ok && flag
}

func manifestReason(raw map[string]any) string {
	reason, _ := raw["maintenance_reason"].(string)
	return reason
}

// installThroughRevision performs the real install sequence through revision
// n (n >= 2), returning the installer for the final revision. Every install
// after the first displaces a prior chain binding.
func installThroughRevision(t *testing.T, f runtimeFixture, held bool, n int) HerdRuntimeInstaller {
	t.Helper()
	owner := heldOwner
	if !held {
		owner = absentOwner
	}
	installer := f.installer
	installer.retention = &RuntimeRetentionOptions{LiveOwner: owner}
	for i := 2; i <= n; i++ {
		next := runtimeNextRevision(t, f, string(rune('0'+i)))
		next.retention = installer.retention
		binding, err := next.Install(context.Background())
		if i == n && held {
			if err == nil || binding == nil || !strings.Contains(err.Error(), "retention maintenance partial") {
				t.Fatalf("install %d: want binding with hard retention partial error, got binding=%+v err=%v", i, binding, err)
			}
		} else if err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
		installer = next
	}
	return installer
}

// Regression for the FAC-794 reviewer repro (59_retention_retry_repro):
// after a real install returns a hard retention-maintenance failure because
// the displaced authenticated backup still had a live owner, an
// exact-revision retry must not silently succeed — it must rerun the bounded
// maintenance or continue returning the hard partial error.
func TestRetentionFailureRemainsVisibleOnExactRevisionRetry(t *testing.T) {
	if !RuntimeInstallSupported() {
		t.Skip("runtime install not supported on this platform")
	}
	f := newRuntimeFixture(t)
	final := installThroughRevision(t, f, true, 4)

	manifest := retentionManifestFor(t, f)
	raw := retentionManifestRawFor(t, f)
	displaced := manifest.Displaced[0]
	if len(manifest.Displaced) != 1 {
		t.Fatalf("fixture invariant: want exactly one displaced binding, got %d", len(manifest.Displaced))
	}
	if _, err := os.Lstat(filepath.Join(f.root, displaced.Path)); err != nil {
		t.Fatalf("displaced backup must still exist: %v", err)
	}

	// The core reviewer invariant first: an exact-revision retry must not
	// silently succeed over the unresolved retention failure.
	binding, err := final.Install(context.Background())
	if err == nil || binding == nil {
		t.Fatalf("exact-revision retry forgot unresolved retention failure: binding=%+v err=%v", binding, err)
	}
	if !strings.Contains(err.Error(), "retention maintenance partial") {
		t.Fatalf("exact-revision retry error = %v", err)
	}
	if !manifestFlagTrue(raw) {
		t.Fatalf("held install must record the unresolved maintenance state")
	}
	after := retentionManifestFor(t, f)
	rawAfter := retentionManifestRawFor(t, f)
	if !manifestFlagTrue(rawAfter) || len(after.Displaced) != 1 || after.Displaced[0].Path != displaced.Path || after.Displaced[0].Digest != displaced.Digest || after.Displaced[0].Revision != displaced.Revision {
		t.Fatalf("retry must retain displaced identity: %+v", after)
	}
	if manifestReason(rawAfter) == "" {
		t.Fatalf("retry must retain the hard partial reason")
	}
}

// The retained failure must resolve once the owner is gone: the retry reruns
// the bounded maintenance, removes exactly the allowlisted displaced binding,
// and clears the unresolved state.
func TestRetainedFailureResolvesWhenOwnerReleased(t *testing.T) {
	if !RuntimeInstallSupported() {
		t.Skip("runtime install not supported on this platform")
	}
	f := newRuntimeFixture(t)
	held := installThroughRevision(t, f, true, 4)

	var displacedPath string
	{
		manifest := retentionManifestFor(t, f)
		displacedPath = filepath.Join(f.root, manifest.Displaced[0].Path)
	}

	released := held
	released.retention = &RuntimeRetentionOptions{LiveOwner: absentOwner}
	binding, err := released.Install(context.Background())
	if err != nil || binding == nil {
		t.Fatalf("released-owner exact-revision retry: binding=%+v err=%v", binding, err)
	}
	if _, statErr := os.Lstat(displacedPath); !os.IsNotExist(statErr) {
		t.Fatalf("resolved displaced binding must be removed, stat=%v", statErr)
	}
	after := retentionManifestFor(t, f)
	rawAfter := retentionManifestRawFor(t, f)
	if manifestFlagTrue(rawAfter) {
		t.Fatalf("resolved state must clear the unresolved flag: %+v", after)
	}
	for _, old := range after.Displaced {
		if old.Path == strings.TrimPrefix(displacedPath, f.root+"/") {
			t.Fatalf("resolved displaced binding must leave the allowlist: %+v", after.Displaced)
		}
	}
}

// A later install must carry unresolved displaced bindings forward with
// their full allowlist identity instead of forgetting them; the forgotten
// identity would make the file permanently unreclaimable.
func TestLaterInstallCarriesUnresolvedDisplacedIdentity(t *testing.T) {
	if !RuntimeInstallSupported() {
		t.Skip("runtime install not supported on this platform")
	}
	f := newRuntimeFixture(t)
	held := installThroughRevision(t, f, true, 4)
	manifest := retentionManifestFor(t, f)
	if len(manifest.Displaced) != 1 {
		t.Fatalf("fixture invariant: want exactly one displaced binding, got %d", len(manifest.Displaced))
	}
	displaced := manifest.Displaced[0]

	next := runtimeNextRevision(t, f, "9")
	next.retention = held.retention
	binding, err := next.Install(context.Background())
	if err == nil || binding == nil || !strings.Contains(err.Error(), "retention maintenance partial") {
		t.Fatalf("later held install: want binding with hard retention partial error, got binding=%+v err=%v", binding, err)
	}
	after := retentionManifestFor(t, f)
	carried := false
	for _, old := range after.Displaced {
		if old.Path == displaced.Path && old.Digest == displaced.Digest && old.Revision == displaced.Revision && old.Size == displaced.Size && old.ModTimeUnixNano == displaced.ModTimeUnixNano {
			carried = true
		}
	}
	if !carried {
		t.Fatalf("later install forgot unresolved displaced identity: before=%+v after=%+v", manifest.Displaced, after.Displaced)
	}
	if _, statErr := os.Lstat(filepath.Join(f.root, displaced.Path)); statErr != nil {
		t.Fatalf("displaced backup must still exist after later install: %v", statErr)
	}
}
