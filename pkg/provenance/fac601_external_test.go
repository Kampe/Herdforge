package provenance

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestFAC601SelectedExecutableCannotBorrowCallerRevision(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/runtime-proof\n\ngo 1.25\n")
	writeFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")
	gitRun(t, root, "init", "-b", "main")
	gitRun(t, root, "config", "user.email", "test@example.com")
	gitRun(t, root, "config", "user.name", "test")
	gitRun(t, root, "add", ".")
	gitRun(t, root, "commit", "-m", "initial binary source")
	oldSHA, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(t.TempDir(), "selected-runtime")
	cmd := exec.Command("go", "build", "-buildvcs=true", "-o", binary, ".")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build fixture: %v\n%s", err, out)
	}
	writeFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() { println(\"new revision\") }\n")
	gitRun(t, root, "add", "main.go")
	gitRun(t, root, "commit", "-m", "new caller revision")
	newSHA, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	oldRevision, oldTime := BinaryRevision, BinaryBuildTime
	BinaryRevision, BinaryBuildTime = newSHA, "2099-01-01T00:00:00Z"
	t.Cleanup(func() { BinaryRevision, BinaryBuildTime = oldRevision, oldTime })
	info, err := ReadExecutable(binary, root)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Comparable {
		t.Fatal("fixture did not prove matching module identity")
	}
	if info.BinaryRevision != oldSHA || info.Current || Validate(info, newSHA) == nil {
		t.Fatalf("stale selected binary borrowed caller identity: %+v; selected=%s caller=%s", info, oldSHA, newSHA)
	}
	if info.BuildTime == BinaryBuildTime {
		t.Fatal("selected binary borrowed caller build time")
	}
	revision, _, _ := binaryValuesFrom(filepath.Join(root, "missing"))
	if strings.TrimSpace(revision) != "" {
		t.Fatal("missing binary borrowed caller revision")
	}
	revision, stamp, _ := binaryValuesFrom("")
	if revision != newSHA || stamp != BinaryBuildTime {
		t.Fatal("running executable lost its own linker metadata")
	}
}

func TestFAC601SelectedLinkerStampRefusesAmbiguousIdentity(t *testing.T) {
	const key = "github.com/Kampe/Herdforge/pkg/provenance.BinaryRevision="
	sha := strings.Repeat("a", 40)
	for _, flags := range []string{"-X " + key + sha, "-X=" + key + sha} {
		revision, _, present := selectedLinkerStamp(flags)
		if !present || revision != sha {
			t.Fatalf("valid native stamp refused: %q", flags)
		}
	}
	for _, flags := range []string{"-X " + key + sha + " -X " + key + sha, "-X " + key + "short", "-X '" + key + sha + "'", key + sha} {
		revision, _, present := selectedLinkerStamp(flags)
		if !present || revision != "" {
			t.Fatalf("ambiguous stamp must be unknown, not fallback: %q", flags)
		}
	}
	if revision, _, present := selectedLinkerStamp("-s -w"); present || revision != "" {
		t.Fatal("unrelated flags became identity")
	}
}
