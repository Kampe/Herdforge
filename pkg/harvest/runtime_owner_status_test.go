package harvest

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeOwnerStatusParsesProductionLsofOutput(t *testing.T) {
	cases := []struct {
		name    string
		script  string
		want    RuntimeOwnerStatus
		wantErr string
	}{
		{"positive owner present", "#!/bin/sh\nprintf 'p4242\\ncsh\\nn%s\\n' \"$4\"\n", RuntimeOwnerPresent, ""},
		{"absent exit one no output", "#!/bin/sh\nexit 1\n", RuntimeOwnerAbsent, ""},
		{"partial empty success", "#!/bin/sh\nexit 0\n", RuntimeOwnerUnknown, "lsof returned empty success"},
		{"partial output with exit one", "#!/bin/sh\necho 'truncated'\nexit 1\n", RuntimeOwnerUnknown, "lsof:"},
		{"lsof hard error", "#!/bin/sh\necho boom >&2\nexit 2\n", RuntimeOwnerUnknown, "lsof:"},
		{"unstructured output", "#!/bin/sh\necho 'p 4242 cat'\n", RuntimeOwnerUnknown, "contradictory record"},
		{"name record for another path", "#!/bin/sh\nprintf 'p4242\\ncsh\\nn/elsewhere\\n'\n", RuntimeOwnerUnknown, "no process holds"},
		{"name record without process", "#!/bin/sh\nprintf 'n%s\\n' \"$4\"\n", RuntimeOwnerUnknown, "no process holds"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			binDir := t.TempDir()
			script := filepath.Join(binDir, "lsof")
			if err := os.WriteFile(script, []byte(tc.script), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir)
			status, err := runtimeOwnerStatus(context.Background(), filepath.Join(t.TempDir(), "candidate"), nil)
			if status != tc.want {
				t.Fatalf("status = %q, want %q (err=%v)", status, tc.want, err)
			}
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want cause containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestAliasCreationRollsBackOnFailedAliasPass(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	r := HerdRuntimeInstaller{Root: root}
	if err := os.Symlink("../elsewhere", filepath.Join(root, "bin", "herdforge")); err != nil {
		t.Fatal(err)
	}
	// The absent herd alias is created, then the pass fails on the foreign
	// bin/herdforge alias; the created alias must not outlive the failure.
	if _, err := r.aliasesCreate(true); err == nil {
		t.Fatal("expected foreign alias refusal")
	}
	if _, statErr := os.Lstat(filepath.Join(root, "herd")); statErr == nil {
		t.Fatal("created alias must be rolled back when the alias pass fails")
	}
	if target, err := os.Readlink(filepath.Join(root, "bin", "herdforge")); err != nil || target != "../elsewhere" {
		t.Fatalf("foreign alias must be untouched evidence: target=%q err=%v", target, err)
	}
}

func TestRemoveCreatedAliasesOnlyRemovesOwnedTargets(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	r := HerdRuntimeInstaller{Root: root}
	created, err := r.aliasesCreate(true)
	if err != nil || len(created) != 2 {
		t.Fatalf("both missing aliases should be created: created=%v err=%v", created, err)
	}
	// Drift one alias off-target before rollback; drifted evidence stays.
	if err := os.Remove(filepath.Join(root, "herd")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../drifted", filepath.Join(root, "herd")); err != nil {
		t.Fatal(err)
	}
	r.removeCreatedAliases(created)
	if _, err := os.Lstat(filepath.Join(root, "herd")); err != nil {
		t.Fatal("drifted alias must be retained as evidence")
	}
	if _, err := os.Lstat(filepath.Join(root, "bin", "herdforge")); !os.IsNotExist(err) {
		t.Fatal("on-target created alias must be rolled back")
	}
}
