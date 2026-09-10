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
		{"positive owner present", "#!/bin/sh\necho 'p 4242 cat'\necho 'f /dev/null'\n", RuntimeOwnerPresent, ""},
		{"absent exit one no output", "#!/bin/sh\nexit 1\n", RuntimeOwnerAbsent, ""},
		{"partial empty success", "#!/bin/sh\nexit 0\n", RuntimeOwnerUnknown, "lsof returned empty success"},
		{"partial output with exit one", "#!/bin/sh\necho 'truncated'\nexit 1\n", RuntimeOwnerUnknown, "lsof:"},
		{"lsof hard error", "#!/bin/sh\necho boom >&2\nexit 2\n", RuntimeOwnerUnknown, "lsof:"},
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
