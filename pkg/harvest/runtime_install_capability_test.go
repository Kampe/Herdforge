package harvest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallRefusesUnsupportedPlatformBeforeMutation(t *testing.T) {
	f := retentionFixture(t)
	target := filepath.Join(f.root, "bin", "herd")
	before, err := os.Lstat(target)
	if err != nil {
		t.Fatal(err)
	}

	orig := runtimeInstallCapability
	runtimeInstallCapability = func() bool { return false }
	defer func() { runtimeInstallCapability = orig }()

	binding, err := f.installer.Install(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unsupported platform") {
		t.Fatalf("unsupported platform must refuse install before mutation, got binding=%+v err=%v", binding, err)
	}

	after, err := os.Lstat(target)
	if err != nil || !os.SameFile(before, after) {
		t.Fatalf("refusal must not mutate the installed target: before=%v after=%v err=%v", before, after, err)
	}
	for _, path := range []string{
		filepath.Join(f.root, ".herd", "runtime-previous"),
		filepath.Join(f.root, ".herd", runtimeRetentionManifestName),
		filepath.Join(f.root, ".herd", runtimeRetentionJournalName),
	} {
		if _, statErr := os.Lstat(path); !os.IsNotExist(statErr) {
			t.Fatalf("refusal must not create %s: %v", path, statErr)
		}
	}
}
