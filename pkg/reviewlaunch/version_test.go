package reviewlaunch

import (
	"errors"
	"strings"
	"testing"
)

func TestRequireRemoteHerdRejectsRevisionDriftBeforeCommandProbe(t *testing.T) {
	req := VersionRequirement{
		RequiredCommand: "capacity",
		LocalRevision:   "4336f1db222d98678881e4d42a43ed108e4f6cdb",
		RemoteRevision:  "29e2d50d2579ee0b94f04ffded528c2f17e16e95",
	}
	err := RequireRemoteHerd(req, "", nil)
	if err == nil {
		t.Fatal("a stale remote herd was admitted before the required command probe")
	}
	for _, want := range []string{"capacity", req.LocalRevision, req.RemoteRevision, "version drift"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("drift diagnostic omitted %q: %v", want, err)
		}
	}
}

func TestRequireRemoteHerdClassifiesUnknownCommandAsVersionDrift(t *testing.T) {
	revision := "4336f1db222d98678881e4d42a43ed108e4f6cdb"
	req := VersionRequirement{RequiredCommand: "capacity", LocalRevision: revision, RemoteRevision: revision}
	err := RequireRemoteHerd(req, "herd: unknown command: capacity", errors.New("exit status 1"))
	if err == nil {
		t.Fatal("unknown required command was admitted")
	}
	var drift *VersionDriftError
	if !errors.As(err, &drift) {
		t.Fatalf("unknown command was reported as a generic launcher error: %v", err)
	}
	for _, want := range []string{"capacity", revision, "version drift"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("unknown-command diagnostic omitted %q: %v", want, err)
		}
	}
}

func TestParseHerdVersion(t *testing.T) {
	const revision = "4336f1db222d98678881e4d42a43ed108e4f6cdb"
	got, err := ParseHerdVersion("herd version 0.2.0-dev (revision " + revision + ", build time unknown)\n")
	if err != nil || got != revision {
		t.Fatalf("ParseHerdVersion() = %q, %v", got, err)
	}
	if _, err := ParseHerdVersion("herd version 0.2.0-dev (revision unknown, build time unknown)"); err == nil {
		t.Fatal("an unidentifiable remote binary was accepted as versioned")
	}
}
