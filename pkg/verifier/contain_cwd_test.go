package verifier

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedCommandStartsInRequestedDirectory(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "cwd-marker")
	if err := os.WriteFile(filepath.Join(dir, "ok.sh"), []byte("#!/bin/sh\nprintf '%s' \"$PWD\" > cwd-marker\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := commandInDir(context.Background(), dir, "sh", "./ok.sh")
	if err := cmd.Run(); err != nil {
		t.Fatalf("owned command failed: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("relative fixture output missing: %v", err)
	}
	if string(got) != dir {
		t.Fatalf("owned command ran in %q, want %q", got, dir)
	}
}
