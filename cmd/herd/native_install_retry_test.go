package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runNativeInstallBuildFixture(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	revision := runNativeInstallGit(t, dir, "rev-parse", "HEAD")
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision="+revision, "-o", "bin/herd", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
}

func runNativeInstallBuildSpy(t *testing.T) (nativeInstallBuild, *bool) {
	t.Helper()
	called := false
	return func(context.Context, string) error {
		called = true
		return nil
	}, &called
}

// lsofOwnerPresent reports whether a live process currently holds path open
// (executing it), using the same structured lsof reader family as retention.
func lsofOwnerPresent(t *testing.T, path string) bool {
	t.Helper()
	cmd := exec.Command("lsof", "-nP", "-Fpcfn", "--", path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), filepath.Base(path))
}

// Regression for the FAC-794 reviewer repro, native CLI path: after an
// install returns a hard retention-maintenance failure with an unresolved
// displaced backup whose live owner still holds it, `herd install --act`
// for the exact same revision must surface the hard partial error — never
// silently succeed over retained unresolved state — and once the owner is
// gone the same retry reruns the bounded maintenance and succeeds.
func TestNativeInstallExactRevisionRetrySurfacesRetainedMaintenanceFailure(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skipf("lsof not found in PATH; skipping native CLI retention retry test: %v", err)
	}
	root := t.TempDir()
	runNativeInstallGit(t, root, "init", "-q", "-b", "main")
	runNativeInstallGit(t, root, "config", "user.email", "test@example.com")
	runNativeInstallGit(t, root, "config", "user.name", "test")
	files := map[string]string{
		"go.mod":     "module example.com/runtime-fixture\n\ngo 1.25\n",
		"main.go":    "package main\n\nimport \"time\"\n\nfunc main() { time.Sleep(300 * time.Second) }\n",
		".gitignore": "bin/\n.herd/\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runNativeInstallGit(t, root, "add", ".")
	runNativeInstallGit(t, root, "commit", "-q", "-m", "old runtime")
	runNativeInstallBuildFixture(t, root)
	source := filepath.Join(t.TempDir(), "source")
	runNativeInstallGit(t, root, "worktree", "add", "-q", "-b", "integration/runtime-fixture", source, "HEAD")
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\n\nimport \"time\"\n\n// integration runtime revision r2 (holder binary source)\nfunc main() { time.Sleep(300 * time.Second) }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runNativeInstallGit(t, source, "add", "main.go")
	runNativeInstallGit(t, source, "commit", "-q", "-m", "new runtime")
	revision := runNativeInstallGit(t, source, "rev-parse", "HEAD")
	runNativeInstallGit(t, root, "update-ref", "refs/remotes/origin/main", revision)
	runNativeInstallBuildFixture(t, source)

	build, called := runNativeInstallBuildSpy(t)
	_ = build
	realBuild := func(ctx context.Context, source string) error {
		runNativeInstallBuildFixture(t, source)
		return nil
	}
	if _, err := executeNativeInstall(context.Background(), nativeInstallOptions{source: source, revision: revision, target: root, act: true}, realBuild); err != nil {
		t.Fatalf("first install: %v", err)
	}

	// Craft the retained unresolved state from the real manifest: the
	// displaced entry is a real copy of the installed binary, executed by a
	// live process so genuine lsof reports a present owner.
	manifestPath := filepath.Join(root, ".herd", "runtime-retention.json")
	body, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("retention manifest: %v", err)
	}
	// UseNumber keeps every numeric literal byte-exact through the rewrite:
	// nanosecond timestamps exceed float64's exact-integer range, and a
	// corrupted mod_time would rightly fail the identity match.
	var manifest map[string]any
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&manifest); err != nil {
		t.Fatalf("retention manifest decode: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(root, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	installedSum := sha256.Sum256(installed)
	displacedName := hex.EncodeToString(installedSum[:])
	displacedPath := filepath.Join(root, ".herd", "runtime-previous", displacedName)
	if err := os.WriteFile(displacedPath, installed, 0o755); err != nil {
		t.Fatal(err)
	}
	st, err := os.Lstat(displacedPath)
	if err != nil {
		t.Fatal(err)
	}
	holder := exec.Command(displacedPath)
	if err := holder.Start(); err != nil {
		t.Fatalf("holder start: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	}()
	deadline := time.Now().Add(10 * time.Second)
	for !lsofOwnerPresent(t, displacedPath) {
		if time.Now().After(deadline) {
			t.Skip("lsof never observed the executing displaced binary; environment does not support this repro")
		}
		time.Sleep(100 * time.Millisecond)
	}
	modNano := st.ModTime().UnixNano()
	manifest["maintenance_unresolved"] = true
	manifest["maintenance_reason"] = "held-unknown-candidate"
	manifest["displaced"] = []map[string]any{{
		"revision":           "r0-holder-probe",
		"digest":             fmt.Sprintf("sha256:%s", displacedName),
		"executable":         "bin/herd",
		"path":               filepath.ToSlash(filepath.Join(".herd", "runtime-previous", displacedName)),
		"size":               json.Number(strconv.FormatInt(st.Size(), 10)),
		"mod_time_unix_nano": json.Number(strconv.FormatInt(modNano, 10)),
	}}
	updated, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, updated, 0o600); err != nil {
		t.Fatal(err)
	}

	*called = false
	if _, err := executeNativeInstall(context.Background(), nativeInstallOptions{source: source, revision: revision, target: root, act: true}, realBuild); err == nil || !strings.Contains(err.Error(), "retention maintenance partial") {
		t.Fatalf("exact-revision CLI retry silently succeeded over unresolved retained maintenance: err=%v", err)
	}
	if *called {
		t.Fatal("exact-revision retry must resolve retained state through the installed binding, not a rebuild")
	}
	if !lsofOwnerPresent(t, displacedPath) {
		t.Fatal("holder must still be live after the failed retry")
	}

	if err := holder.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_, _ = holder.Process.Wait()
	if lsofOwnerPresent(t, displacedPath) {
		journal, _ := os.ReadFile(filepath.Join(root, ".herd", "runtime-retention.jsonl"))
		t.Fatalf("holder still observed after kill: journal=%s", journal)
	}
	if _, err := executeNativeInstall(context.Background(), nativeInstallOptions{source: source, revision: revision, target: root, act: true}, realBuild); err != nil {
		manifestDump, _ := os.ReadFile(manifestPath)
		journal, _ := os.ReadFile(filepath.Join(root, ".herd", "runtime-retention.jsonl"))
		prevEntries, _ := os.ReadDir(filepath.Join(root, ".herd", "runtime-previous"))
		names := make([]string, 0, len(prevEntries))
		for _, e := range prevEntries {
			names = append(names, e.Name())
		}
		t.Fatalf("released-owner CLI retry must resolve the retained failure: %v\nmanifest=%s\nprevious=%v\njournal=%s", err, manifestDump, names, journal)
	}
}
