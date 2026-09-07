package harvest

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type runtimeFixture struct {
	root, source, oldSHA, newSHA string
	installer                    HerdRuntimeInstaller
}

func runtimeGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %q: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func runtimeBuild(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	revision := runtimeGit(t, dir, "rev-parse", "HEAD")
	cmd := exec.Command("go", "build", "-buildvcs=true", "-ldflags", "-X github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision="+revision, "-o", "bin/herd", ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
}
func newRuntimeFixture(t *testing.T) runtimeFixture {
	t.Helper()
	root := t.TempDir()
	for name, body := range map[string]string{"go.mod": "module example.com/runtime-fixture\n\ngo 1.25\n", "main.go": "package main\nfunc main() {}\n", ".gitignore": "bin/\nherd\n.herd/\n"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	runtimeGit(t, root, "init", "-b", "main")
	runtimeGit(t, root, "config", "user.email", "test@example.com")
	runtimeGit(t, root, "config", "user.name", "test")
	runtimeGit(t, root, "add", ".")
	runtimeGit(t, root, "commit", "-m", "old runtime")
	old := runtimeGit(t, root, "rev-parse", "HEAD")
	runtimeBuild(t, root)
	source := filepath.Join(t.TempDir(), "source")
	runtimeGit(t, root, "worktree", "add", "-b", "integration/runtime-fixture", source, "HEAD")
	if err := os.WriteFile(filepath.Join(source, "main.go"), []byte("package main\nfunc main() { println(\"new\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, source, "add", "main.go")
	runtimeGit(t, source, "commit", "-m", "new runtime")
	current := runtimeGit(t, source, "rev-parse", "HEAD")
	runtimeGit(t, root, "update-ref", "refs/remotes/origin/main", current)
	runtimeBuild(t, source)
	return runtimeFixture{root, source, old, current, HerdRuntimeInstaller{Root: root, Source: source, Revision: current}}
}
func TestFAC601RuntimeInstallAndResume(t *testing.T) {
	f := newRuntimeFixture(t)
	ctx := context.Background()
	prior, err := os.Stat(filepath.Join(f.root, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Observe(ctx); err == nil {
		t.Fatal("old runtime unexpectedly bound")
	}
	bound, err := f.installer.Install(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Revision != f.newSHA || !strings.HasPrefix(bound.Digest, "sha256:") || len(bound.Digest) != 71 || bound.Executable != "bin/herd" {
		t.Fatalf("bad binding: %+v", bound)
	}
	installed, err := os.Stat(filepath.Join(f.root, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(prior, installed) {
		t.Fatal("installation did not atomically replace old inode")
	}
	backups, err := os.ReadDir(filepath.Join(f.root, ".herd", "runtime-previous"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("prior runtime not preserved: %v %v", backups, err)
	}
	backup, err := os.Stat(filepath.Join(f.root, ".herd", "runtime-previous", backups[0].Name()))
	if err != nil || !os.SameFile(prior, backup) {
		t.Fatal("backup lost prior inode")
	}
	again, err := f.installer.Install(ctx)
	if err != nil || *again != *bound {
		t.Fatalf("resume changed proof: %+v %v", again, err)
	}
	final, err := os.Stat(filepath.Join(f.root, "bin", "herd"))
	if err != nil || !os.SameFile(installed, final) {
		t.Fatal("resume reinstalled an already-bound runtime")
	}
	for _, alias := range []string{"herd", "bin/herdforge"} {
		st, err := os.Stat(filepath.Join(f.root, alias))
		if err != nil || !os.SameFile(final, st) {
			t.Fatalf("alias %s does not select bound inode", alias)
		}
	}
}
func TestFAC601RuntimeRefusesUnlandedDirtyAndRedirectedInputs(t *testing.T) {
	f := newRuntimeFixture(t)
	target := filepath.Join(f.root, "bin", "herd")
	before, _ := os.Stat(target)
	runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", f.oldSHA)
	if _, err := f.installer.Install(context.Background()); err == nil {
		t.Fatal("unlanded revision installed")
	}
	runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", f.newSHA)
	if err := os.WriteFile(filepath.Join(f.source, "main.go"), []byte("package main\nfunc main() { panic(\"dirty\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Install(context.Background()); err == nil {
		t.Fatal("dirty source installed")
	}
	runtimeGit(t, f.source, "restore", "main.go")
	untracked := filepath.Join(f.source, "untracked.go")
	if err := os.WriteFile(untracked, []byte("package main\nvar surprise = 1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "dirty") {
		t.Fatalf("untracked source admitted: %v", err)
	}
	if err := os.Remove(untracked); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("elsewhere", filepath.Join(f.root, "herd")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Install(context.Background()); err == nil {
		t.Fatal("redirected consumer alias overwritten")
	}
	after, _ := os.Stat(target)
	if !os.SameFile(before, after) {
		t.Fatal("refusal changed active runtime")
	}
}
func TestFAC601RuntimeRefusesStaleBuiltArtifact(t *testing.T) {
	f := newRuntimeFixture(t)
	old, err := os.ReadFile(filepath.Join(f.root, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.source, "bin", "herd"), old, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Install(context.Background()); err == nil {
		t.Fatal("stale artifact installed as current source")
	}
}

func TestFAC601RuntimeRefusesDowngrade(t *testing.T) {
	f := newRuntimeFixture(t)
	artifact, err := os.ReadFile(filepath.Join(f.source, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.source, "main.go"), []byte("package main\nfunc main() { println(\"newest\") }\n"), 0644); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, f.source, "add", "main.go")
	runtimeGit(t, f.source, "commit", "-m", "newer installed runtime")
	newest := runtimeGit(t, f.source, "rev-parse", "HEAD")
	runtimeBuild(t, f.source)
	newer, err := os.ReadFile(filepath.Join(f.source, "bin", "herd"))
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(f.root, "bin", "herd")
	if err := os.WriteFile(target, newer, 0755); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", newest)
	runtimeGit(t, f.source, "checkout", "--detach", f.newSHA)
	if err := os.WriteFile(filepath.Join(f.source, "bin", "herd"), artifact, 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.Install(context.Background()); err == nil || !strings.Contains(err.Error(), "downgrade") {
		t.Fatalf("expected downgrade refusal, got %v", err)
	}
	after, err := os.ReadFile(target)
	if err != nil || string(after) != string(newer) {
		t.Fatal("downgrade refusal changed newer installed runtime")
	}
}

func TestFAC601RuntimeObservationDistinguishesPendingFromUnknown(t *testing.T) {
	f := newRuntimeFixture(t)
	ctx := context.Background()
	if got, err := f.installer.ObserveInstallation(ctx); err != nil || got != nil {
		t.Fatalf("proven ancestor runtime should await installation: %+v %v", got, err)
	}
	binding, err := f.installer.Install(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := f.installer.ObserveInstallation(ctx); err != nil || got == nil || got.Digest != binding.Digest {
		t.Fatalf("installed runtime not observed: %+v %v", got, err)
	}
	if err := os.Remove(filepath.Join(f.root, "herd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("foreign", filepath.Join(f.root, "herd")); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.ObserveInstallation(ctx); err == nil {
		t.Fatal("foreign alias was classified as an unapplied installation")
	}
	if err := os.Remove(filepath.Join(f.root, "herd")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, "bin", "herd"), []byte("not a Go executable"), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err := f.installer.ObserveInstallation(ctx); err == nil {
		t.Fatal("unknown executable was classified as an unapplied installation")
	}
}
