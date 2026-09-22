package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRefuseLiveCwdOwnerDoesNotPassJSONFlag(t *testing.T) {
	dir := t.TempDir()
	argsPath := filepath.Join(dir, "herdr.args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + argsPath + "\"\nprintf '%s\\n' '{\"id\":\"cli:agent:list\",\"result\":{\"agents\":[]}}'\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	prev := liveOwnerProbe
	liveOwnerProbe = nil
	t.Cleanup(func() { liveOwnerProbe = prev })

	if err := refuseLiveCwdOwner(t.TempDir()); err != nil {
		t.Fatalf("empty roster must not refuse: %v", err)
	}
	raw, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Fields(strings.TrimSpace(string(raw)))
	hasList, hasJSON := false, false
	for _, a := range args {
		if a == "list" {
			hasList = true
		}
		if a == "--json" {
			hasJSON = true
		}
	}
	if !hasList {
		t.Fatalf("herdr argv missing list: %v", args)
	}
	if hasJSON {
		t.Fatalf("herdr 0.9.0 rejects --json; argv was %v", args)
	}
}

func TestRefuseLiveCwdOwnerUnknownOutputFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\necho not-json\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	prev := liveOwnerProbe
	liveOwnerProbe = nil
	t.Cleanup(func() { liveOwnerProbe = prev })
	err := refuseLiveCwdOwner(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "refusing unknown live owners") {
		t.Fatalf("unparseable herdr list must fail closed: %v", err)
	}
}

func TestRefuseLiveCwdOwnerFailedListFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte("#!/bin/sh\nexit 2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	prev := liveOwnerProbe
	liveOwnerProbe = nil
	t.Cleanup(func() { liveOwnerProbe = prev })
	err := refuseLiveCwdOwner(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "herdr agent list failed") {
		t.Fatalf("failed herdr list must fail closed: %v", err)
	}
}

func TestRefuseLiveCwdOwnerMatchingCwdRefuses(t *testing.T) {
	target := t.TempDir()
	dir := t.TempDir()
	body := "#!/bin/sh\nprintf '%s\\n' '{\"result\":{\"agents\":[{\"cwd\":\"" + target + "\",\"foreground_cwd\":\"\"}]}}'\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	prev := liveOwnerProbe
	liveOwnerProbe = nil
	t.Cleanup(func() { liveOwnerProbe = prev })
	err := refuseLiveCwdOwner(target)
	if err == nil || !strings.Contains(err.Error(), "herdr process cwd owns") {
		t.Fatalf("matching cwd must refuse: %v", err)
	}
}
