package resources

// FAC-829: exclusivity and persistence. A second observer must refuse promptly
// rather than wait, and must never publish over the report of the one that
// already owns the lock.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// refusingLock stands in for a held lock, so the refusal contract can be tested
// on every platform including the ones where flock is unavailable.
type refusingLock struct{ err error }

func (r refusingLock) Acquire(context.Context, string, time.Duration, time.Duration) (io.Closer, error) {
	return nil, r.err
}

// TestRunObserverRefusesWhenTheLockIsHeld proves a competing observer fails
// rather than queueing behind the incumbent.
func TestRunObserverRefusesWhenTheLockIsHeld(t *testing.T) {
	t.Setenv("HERD_STATE_DIR", t.TempDir())
	cfg := DefaultObserverConfig()

	_, err := RunObserver(context.Background(), cfg, refusingLock{err: errors.New("not acquired within 2s")})
	if err == nil {
		t.Fatalf("a held lock produced no error; two observers would publish over each other")
	}
	if !errors.Is(err, ErrObserverBusy) {
		t.Fatalf("refusal was %v, expected it to wrap ErrObserverBusy", err)
	}
	if !strings.Contains(err.Error(), string(ObserverLockScope())) {
		t.Fatalf("refusal %q does not state the exclusivity scope actually in force", err)
	}
}

// TestRunObserverRefusalDoesNotTouchTheStatusFile is the "must not overwrite
// another observer's report" rule, tested rather than asserted.
func TestRunObserverRefusalDoesNotTouchTheStatusFile(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_STATE_DIR", root)
	cfg := DefaultObserverConfig()

	incumbent := []byte(`{"schema_version":1,"observer":{"pid":4242}}`)
	if err := os.MkdirAll(filepath.Dir(cfg.StatusPath), 0o700); err != nil {
		t.Fatalf("prepare status directory: %v", err)
	}
	if err := os.WriteFile(cfg.StatusPath, incumbent, 0o600); err != nil {
		t.Fatalf("write incumbent status: %v", err)
	}

	if _, err := RunObserver(context.Background(), cfg, refusingLock{err: errors.New("held")}); err == nil {
		t.Fatalf("expected the refused observer to return an error")
	}

	after, err := os.ReadFile(cfg.StatusPath)
	if err != nil {
		t.Fatalf("read status after refusal: %v", err)
	}
	if string(after) != string(incumbent) {
		t.Fatalf("a refused observer rewrote the incumbent's report:\n got %s\nwant %s", after, incumbent)
	}
}

// TestFileLockProviderRefusesASecondHolderPromptly exercises the real flock on
// the platforms that have one. It is bounded: the second acquisition must fail
// well inside its wait, never hang.
func TestFileLockProviderRefusesASecondHolderPromptly(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "linux", "freebsd", "netbsd", "openbsd":
	default:
		t.Skipf("flock is unavailable on %s", runtime.GOOS)
	}
	path := filepath.Join(t.TempDir(), "observer", "resources-observer.lock")

	first, err := FileLockProvider{}.Acquire(context.Background(), path, time.Second, 100*time.Millisecond)
	if err != nil {
		t.Fatalf("first acquisition failed: %v", err)
	}
	defer first.Close()

	started := time.Now()
	second, err := FileLockProvider{}.Acquire(context.Background(), path, 500*time.Millisecond, 50*time.Millisecond)
	elapsed := time.Since(started)
	if err == nil {
		second.Close()
		t.Fatalf("two holders acquired the same observer lock; exclusivity is not real")
	}
	if elapsed > 5*time.Second {
		t.Fatalf("the second holder waited %s before refusing; a competing observer must fail promptly", elapsed)
	}
}

// TestObserverStatusRoundTripsAtomically proves a published status reads back
// exactly, and that the write replaces the file rather than appending to it.
func TestObserverStatusRoundTripsAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observer", "resources-observer.json")
	status := ObserverStatus{
		SchemaVersion: ObserverSchemaVersion,
		PublishedAt:   "2026-09-12T22:31:05Z",
		ExpiresAt:     "2026-09-12T22:32:35Z",
		Latest:        ObserverSample{Sequence: 7, CompletedAt: "2026-09-12T22:30:50Z"},
		TotalTicks:    7,
		Authority:     ObserverAuthorityNote,
	}
	if err := writeObserverStatus(path, status); err != nil {
		t.Fatalf("write status: %v", err)
	}
	// A second write must replace, not grow, the artifact.
	if err := writeObserverStatus(path, status); err != nil {
		t.Fatalf("rewrite status: %v", err)
	}

	got, err := ReadObserverStatus(path)
	if !observerReadSupported {
		if !errors.Is(err, ErrObserverReadUnsupported) {
			t.Fatalf("on a platform without a bounded open the read must refuse as unsupported, got %v", err)
		}
		return
	}
	if err != nil {
		t.Fatalf("read status: %v", err)
	}
	if got.Latest.CompletedAt != status.Latest.CompletedAt {
		t.Fatalf("observation time did not round-trip: got %q want %q",
			got.Latest.CompletedAt, status.Latest.CompletedAt)
	}
	if got.PublishedAt != status.PublishedAt {
		t.Fatalf("publish time did not round-trip: got %q want %q", got.PublishedAt, status.PublishedAt)
	}
	if got.Authority == "" {
		t.Fatalf("the authority disclaimer did not survive publication")
	}

	// No temporary files may be left behind next to the artifact.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("list status directory: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".resource-observer-") {
			t.Fatalf("a temporary file %q was left behind", e.Name())
		}
	}
}

// TestReadObserverStatusRefusesAnOversizedFile proves the SIZE bound is what
// rejects an oversized file.
//
// The fixture is deliberately VALID: a well-formed, supported-schema status
// padded with legal JSON whitespace to exactly MaxObserverStatusBytes+1. A
// fixture of raw spaces would have been rejected by the decoder instead, so
// removing the size check entirely would still produce an error and the control
// would prove nothing.
func TestReadObserverStatusRefusesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resources-observer.json")
	body := oversizeValidStatus(t)
	if len(body) != MaxObserverStatusBytes+1 {
		t.Fatalf("fixture is %d bytes, expected exactly %d", len(body), MaxObserverStatusBytes+1)
	}
	// The padded fixture must still be valid JSON of the supported schema, or
	// this test would pass for the wrong reason.
	var probe ObserverStatus
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("fixture is not valid JSON: %v", err)
	}
	if probe.SchemaVersion != ObserverSchemaVersion {
		t.Fatalf("fixture carries schema %d, expected the supported %d", probe.SchemaVersion, ObserverSchemaVersion)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write oversized status: %v", err)
	}

	_, err := ReadObserverStatus(path)
	if err == nil {
		t.Fatalf("an oversized status file was read; the size bound is not enforced")
	}
	if !observerReadSupported {
		if !errors.Is(err, ErrObserverReadUnsupported) {
			t.Fatalf("on a platform without a bounded open the read must refuse as unsupported, got %v", err)
		}
		return
	}
	if !strings.Contains(err.Error(), "exceeds the") {
		t.Fatalf("a valid oversized status was rejected by something other than the size bound: %v", err)
	}
}

// oversizeValidStatus builds a valid status padded with legal whitespace to
// exactly one byte past the bound.
func oversizeValidStatus(t *testing.T) []byte {
	t.Helper()
	body, err := json.Marshal(ObserverStatus{SchemaVersion: ObserverSchemaVersion})
	if err != nil {
		t.Fatalf("encode fixture status: %v", err)
	}
	want := MaxObserverStatusBytes + 1
	if len(body) >= want {
		t.Fatalf("the minimal status is already %d bytes, at or past the %d bound", len(body), want)
	}
	padding := make([]byte, want-len(body))
	for i := range padding {
		padding[i] = ' '
	}
	return append(body, padding...)
}

// TestReadObserverStatusRefusesAnUnknownSchema keeps a future incompatible
// shape from being half-interpreted by today's reader.
func TestReadObserverStatusRefusesAnUnknownSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resources-observer.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":999}`), 0o600); err != nil {
		t.Fatalf("write status: %v", err)
	}
	_, err := ReadObserverStatus(path)
	if err == nil {
		t.Fatalf("an unrecognised schema version was accepted")
	}
	if !observerReadSupported && !errors.Is(err, ErrObserverReadUnsupported) {
		t.Fatalf("on a platform without a bounded open the read must refuse as unsupported, got %v", err)
	}
}
