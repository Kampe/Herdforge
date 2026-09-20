package harvest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

const landedIgnoreRule = "/.herd/landed/\n"

// This fixture deliberately does not use newRuntimeFixture: its blanket .herd/
// ignore hides both the missing landed rule and accidentally hidden source.
// No executable is built; validate is the production pre-install source gate.
func newLandedIgnoreFixture(t *testing.T, policy string) runtimeFixture {
	t.Helper()
	for _, name := range []string{"GIT_CONFIG_GLOBAL", "GIT_CONFIG_SYSTEM"} {
		t.Setenv(name, os.DevNull)
	}
	t.Setenv("GIT_CONFIG_COUNT", "0")
	parent := t.TempDir()
	root := filepath.Join(parent, "repo")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	runtimeGit(t, root, "init", "--template=", "-b", "main")
	for _, kv := range [][2]string{
		{"user.name", "test"}, {"user.email", "test@example.invalid"},
		{"commit.gpgsign", "false"}, {"core.hooksPath", os.DevNull},
		{"core.excludesFile", os.DevNull},
	} {
		runtimeGit(t, root, "config", kv[0], kv[1])
	}
	writeLandedIgnoreFile(t, root, ".gitignore", policy)
	writeLandedIgnoreFile(t, root, "source.txt", "tracked source\n")
	runtimeGit(t, root, "add", ".gitignore", "source.txt")
	runtimeGit(t, root, "commit", "-m", "source with repository ignore policy")
	base := runtimeGit(t, root, "rev-parse", "HEAD")
	source := filepath.Join(parent, "source")
	runtimeGit(t, root, "worktree", "add", "--detach", source, base)
	runtimeGit(t, root, "update-ref", "refs/remotes/origin/main", base)
	return runtimeFixture{root, source, base, base, HerdRuntimeInstaller{Root: root, Source: source, Revision: base}}
}

func writeLandedIgnoreFile(t *testing.T, root, name, body string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setLandedIgnorePolicy(t *testing.T, f *runtimeFixture, policy string) {
	t.Helper()
	writeLandedIgnoreFile(t, f.source, ".gitignore", policy)
	// Commit the policy so a tracked .gitignore modification cannot masquerade
	// as the missing-rule failure. Update the native gate's exact landed input.
	runtimeGit(t, f.source, "add", ".gitignore")
	runtimeGit(t, f.source, "commit", "-m", "exercise ignore policy")
	f.newSHA = runtimeGit(t, f.source, "rev-parse", "HEAD")
	f.installer.Revision = f.newSHA
	runtimeGit(t, f.root, "update-ref", "refs/remotes/origin/main", f.newSHA)
}

// The returned string is a named behavioral assertion, never a setup/command
// error. Both controls must trip the same oracle used for the real policy;
// unrelated Git, writer, readback or validation failures fail the test outright.
func landedIgnoreOracle(t *testing.T, f runtimeFixture) string {
	t.Helper()
	written, err := hsync.WriteLandedDisposition(f.source, hsync.LandedDisposition{
		Ref: "FAC-834", CandidateSHA: f.oldSHA, MergeSHA: f.newSHA,
		Method: hsync.LandedByAncestry, Actor: "landed-ignore-fixture",
	})
	if err != nil {
		t.Fatalf("write native disposition: %v", err)
	}
	read, err := hsync.ReadLandedDisposition(f.source, "FAC-834")
	if err != nil || read == nil || *read != *written || read.CandidateSHA != f.oldSHA || read.MergeSHA != f.newSHA {
		t.Fatalf("native disposition lost its exact identity: %+v, %v", read, err)
	}
	if err := f.installer.validate(context.Background()); err != nil {
		path, relErr := filepath.Rel(f.source, hsync.LandedPath(f.source, "FAC-834"))
		if relErr != nil {
			t.Fatal(relErr)
		}
		status := runtimeGit(t, f.source, "status", "--porcelain", "--untracked-files=all")
		if err.Error() != "runtime bind: source is dirty or unreadable" || status != "?? "+filepath.ToSlash(path) {
			t.Fatalf("disposition cleanliness failed for an unrelated reason: %v; status %q", err, status)
		}
		return "landed disposition dirtied the source checkout"
	}
	for _, path := range []string{".herd/unexpected-source.txt", "unexpected-source.txt", "nested/.herd/landed/source.txt"} {
		if failure := landedIgnoreUntrackedOracle(t, f, path); failure != "" {
			return failure
		}
	}
	return landedIgnoreTrackedOracle(t, f)
}

func landedIgnoreTrackedOracle(t *testing.T, f runtimeFixture) string {
	t.Helper()
	writeLandedIgnoreFile(t, f.source, "source.txt", "modified tracked source\n")
	err := f.installer.validate(context.Background())
	writeLandedIgnoreFile(t, f.source, "source.txt", "tracked source\n")
	if err == nil {
		return "tracked source modification was admitted"
	}
	if err.Error() != "runtime bind: source is dirty or unreadable" {
		t.Fatalf("tracked source refused for an unrelated reason: %v", err)
	}
	if err := f.installer.validate(context.Background()); err != nil {
		t.Fatalf("restored source eligibility: %v", err)
	}
	return ""
}

func landedIgnoreUntrackedOracle(t *testing.T, f runtimeFixture, path string) string {
	t.Helper()
	writeLandedIgnoreFile(t, f.source, path, "unexpected source\n")
	err := f.installer.validate(context.Background())
	status := runtimeGit(t, f.source, "status", "--porcelain", "--untracked-files=all", "--", path)
	if removeErr := os.Remove(filepath.Join(f.source, path)); removeErr != nil {
		t.Fatal(removeErr)
	}
	if err != nil && err.Error() != "runtime bind: source is dirty or unreadable" {
		t.Fatalf("untracked source refused for an unrelated reason: %v", err)
	}
	if err == nil && status == "" {
		return "unrelated source hidden or admitted: " + path
	}
	if status != "?? "+path {
		t.Fatalf("untracked source refused for an unrelated reason: %v; status %q", err, status)
	}
	if err == nil {
		return "visible untracked source was admitted: " + path
	}
	return ""
}

func TestLandedDispositionPreservesSourceEligibility(t *testing.T) {
	// Go package tests run from the package directory. Read the checked-out
	// repository policy, not a handwritten imitation or a local Git exclude.
	policyBytes, err := os.ReadFile(filepath.Join("..", "..", ".gitignore"))
	if err != nil {
		t.Fatal(err)
	}
	policy := string(policyBytes)
	if strings.Count(policy, landedIgnoreRule) != 1 {
		t.Fatal("expected one narrow root-anchored landed ignore rule")
	}
	f := newLandedIgnoreFixture(t, policy)
	if err := f.installer.validate(context.Background()); err != nil {
		t.Fatalf("fixture was ineligible before native output: %v", err)
	}
	if failure := landedIgnoreOracle(t, f); failure != "" {
		t.Fatal(failure)
	}
	for _, control := range []struct{ name, replacement, want string }{
		{"missing-rule", "", "landed disposition dirtied the source checkout"},
		{"overbroad-rule", "/.herd/\n", "unrelated source hidden or admitted: .herd/unexpected-source.txt"},
	} {
		t.Run(control.name, func(t *testing.T) {
			setLandedIgnorePolicy(t, &f, strings.Replace(policy, landedIgnoreRule, control.replacement, 1))
			if failure := landedIgnoreOracle(t, f); failure != control.want {
				t.Fatalf("control did not fail its causal assertion: got %q, want %q", failure, control.want)
			}
			setLandedIgnorePolicy(t, &f, policy)
			if failure := landedIgnoreOracle(t, f); failure != "" {
				t.Fatalf("restored policy: %s", failure)
			}
		})
	}
}
