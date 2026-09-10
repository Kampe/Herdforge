package provenance

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildHerdCrossDirectorySourceProvenance proves that scripts/build-herd.sh
// compiles the source tree from the target repository directory passed in $1
// and embeds that target's true Git VCS revision in debug.BuildInfo, rather than
// compiling from whatever caller directory the script was launched from.
func TestBuildHerdCrossDirectorySourceProvenance(t *testing.T) {
	buildScript, err := filepath.Abs("../../scripts/build-herd.sh")
	if err != nil {
		t.Fatalf("resolve build script: %v", err)
	}
	if _, err := os.Stat(buildScript); err != nil {
		t.Fatalf("build script %s not found: %v", buildScript, err)
	}

	// 1. Create a distinct caller repo (Caller Root) with its own go.mod and a unique marker
	callerDir := filepath.Join(t.TempDir(), "caller repo with spaces")
	mustMkdir(t, filepath.Join(callerDir, "cmd", "herd"))
	writeFile(t, filepath.Join(callerDir, "go.mod"), "module github.com/Kampe/Herdforge\n\ngo 1.25\n")
	writeFile(t, filepath.Join(callerDir, "cmd", "herd", "main.go"), `package main
import (
	"fmt"
	"os"
	"flag"
	"github.com/Kampe/Herdforge/pkg/provenance"
)
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("herd version 0.2.0-dev (revision %s, marker CALLER_REPO)\n", provenance.BinaryRevision)
		return
	}
	flag.Parse()
	fmt.Println("CALLER_REPO")
}
`)
	mustMkdir(t, filepath.Join(callerDir, "pkg", "provenance"))
	writeFile(t, filepath.Join(callerDir, "pkg", "provenance", "provenance.go"), `package provenance
var BinaryRevision = ""
var BinaryBuildTime = ""
`)

	gitInitAndCommit(t, callerDir, "initial commit in caller repo")
	callerSHA, err := gitValue(callerDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("caller rev-parse: %v", err)
	}

	// 2. Create a distinct target repo (Target Root) with its own unique marker and git history
	targetDir := filepath.Join(t.TempDir(), "target repo with spaces")
	mustMkdir(t, filepath.Join(targetDir, "cmd", "herd"))
	writeFile(t, filepath.Join(targetDir, "go.mod"), "module github.com/Kampe/Herdforge\n\ngo 1.25\n")
	writeFile(t, filepath.Join(targetDir, "cmd", "herd", "main.go"), `package main
import (
	"fmt"
	"os"
	"flag"
	"github.com/Kampe/Herdforge/pkg/provenance"
)
func main() {
	if len(os.Args) > 1 && os.Args[1] == "--version" {
		fmt.Printf("herd version 0.2.0-dev (revision %s, marker TARGET_REPO)\n", provenance.BinaryRevision)
		return
	}
	flag.Parse()
	fmt.Println("TARGET_REPO")
}
`)
	mustMkdir(t, filepath.Join(targetDir, "pkg", "provenance"))
	writeFile(t, filepath.Join(targetDir, "pkg", "provenance", "provenance.go"), `package provenance
var BinaryRevision = ""
var BinaryBuildTime = ""
`)

	gitInitAndCommit(t, targetDir, "initial commit in target repo")
	targetSHA, err := gitValue(targetDir, "rev-parse", "HEAD")
	if err != nil {
		t.Fatalf("target rev-parse: %v", err)
	}

	if callerSHA == targetSHA {
		t.Fatalf("test setup failure: callerSHA (%s) and targetSHA (%s) must be distinct", callerSHA, targetSHA)
	}

	// 3. Execute scripts/build-herd.sh from CALLER directory, passing TARGET directory as $1
	cmd := exec.Command("/bin/sh", buildScript, targetDir)
	cmd.Dir = callerDir // Cross-directory invocation: caller is in callerDir!
	cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build-herd.sh failed: %v\nOutput:\n%s", err, string(out))
	}

	targetBin := filepath.Join(targetDir, "bin", "herd")
	if _, err := os.Stat(targetBin); err != nil {
		t.Fatalf("target binary not created at %s: %v", targetBin, err)
	}

	// 4. Verify executed binary behavior: must output TARGET_REPO, never CALLER_REPO
	runCmd := exec.Command(targetBin)
	runOut, err := runCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("run target binary: %v\nOutput: %s", err, string(runOut))
	}
	if !strings.Contains(string(runOut), "TARGET_REPO") {
		t.Fatalf("compiled binary content mismatch: expected 'TARGET_REPO', got %q", string(runOut))
	}
	if strings.Contains(string(runOut), "CALLER_REPO") {
		t.Fatalf("compiled binary contained foreign caller code ('CALLER_REPO')!")
	}

	// 5. Verify debug.BuildInfo embedded VCS metadata: vcs.revision must match targetSHA, not callerSHA
	bi, err := buildinfo.ReadFile(targetBin)
	if err != nil {
		t.Fatalf("read buildinfo from %s: %v", targetBin, err)
	}

	var embeddedVCSRevision string
	for _, setting := range bi.Settings {
		if setting.Key == "vcs.revision" {
			embeddedVCSRevision = setting.Value
			break
		}
	}

	if embeddedVCSRevision == "" {
		t.Fatalf("binary has no embedded vcs.revision setting in buildinfo")
	}

	if embeddedVCSRevision != targetSHA {
		t.Fatalf("vcs.revision mismatch: binary embedded caller revision %s instead of target revision %s", embeddedVCSRevision, targetSHA)
	}

	// 6. Verify symlinks and relative path integrity
	symlinkPath := filepath.Join(targetDir, "herd")
	if dest, err := os.Readlink(symlinkPath); err != nil || dest != "bin/herd" {
		t.Fatalf("expected symlink %s -> bin/herd, got dest %q (err: %v)", symlinkPath, dest, err)
	}
	herdforgeSymlink := filepath.Join(targetDir, "bin", "herdforge")
	if dest, err := os.Readlink(herdforgeSymlink); err != nil || dest != "herd" {
		t.Fatalf("expected symlink %s -> herd, got dest %q (err: %v)", herdforgeSymlink, dest, err)
	}
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func gitInitAndCommit(t *testing.T, dir, msg string) {
	t.Helper()
	gitRun(t, dir, "init", "-b", "main")
	gitRun(t, dir, "config", "user.email", "test@example.com")
	gitRun(t, dir, "config", "user.name", "test")
	gitRun(t, dir, "add", ".")
	gitRun(t, dir, "commit", "-m", msg)
}
