package preflight

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFenceBrokerAbsenceIsReportedBeforeWork is the FAC-563 gate.
//
// The broker is required infrastructure for any fenced board write, and nothing
// said so before the write failed: classed internal in the control surface,
// absent from docs, unchecked by any readiness gate. A restored checkout has
// fence state and no broker, and the first symptom was a failed close mid
// mutation.
func TestFenceBrokerAbsenceIsReportedBeforeWork(t *testing.T) {
	for _, key := range []string{"HERD_FENCE_COORDINATOR", "HERD_FENCE_BROKER_URL", "HERD_FENCE_ATOMIC_SERVER"} {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	r := CheckFenceBroker(t.TempDir())
	if r.Ready {
		t.Fatal("with no broker and no native fencing, a fenced write cannot succeed")
	}
	if r.Remedy == "" {
		t.Fatal("an unmet requirement must carry the command that satisfies it")
	}
	// Every supported route must be named, or an operator picks by guessing.
	for _, want := range []string{
		"HERD_FENCE_COORDINATOR=1",
		"herd fence-broker",
		"HERD_FENCE_BROKER_URL",
		"HERD_FENCE_ATOMIC_SERVER=1",
	} {
		if !strings.Contains(r.Remedy, want) {
			t.Errorf("remedy must name %q", want)
		}
	}
	// Stock Kaneo must be explicitly excluded, since assuming otherwise is what
	// makes the third route look like a free pass.
	if !strings.Contains(r.Remedy, "Stock Kaneo does NOT qualify") {
		t.Error("remedy must say stock Kaneo cannot enforce fence+op-dedupe")
	}
}

// Existing fence state with no broker is the exact trap: the checkout looks
// provisioned and is not. The message must say so specifically.
func TestExistingFenceStateWithoutBrokerIsCalledOut(t *testing.T) {
	for _, key := range []string{"HERD_FENCE_COORDINATOR", "HERD_FENCE_BROKER_URL", "HERD_FENCE_ATOMIC_SERVER"} {
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fences.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := CheckFenceBroker(dir)
	if r.Ready {
		t.Fatal("fence state is not a broker")
	}
	if !strings.Contains(r.Detail, "fence state already exists") {
		t.Errorf("detail must name the trap, got %q", r.Detail)
	}
}

// Each supported posture must read as ready, so the check does not nag an
// operator who has already satisfied it.
func TestEachSupportedPostureReadsReady(t *testing.T) {
	cases := map[string][2]string{
		"coordinator hosts in-process": {"HERD_FENCE_COORDINATOR", "1"},
		"standalone broker configured":  {"HERD_FENCE_BROKER_URL", "unix:///tmp/x.sock"},
		"native atomic board":           {"HERD_FENCE_ATOMIC_SERVER", "1"},
	}
	for name, kv := range cases {
		t.Run(name, func(t *testing.T) {
			for _, key := range []string{"HERD_FENCE_COORDINATOR", "HERD_FENCE_BROKER_URL", "HERD_FENCE_ATOMIC_SERVER"} {
				if err := os.Unsetenv(key); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv(kv[0], kv[1])
			r := CheckFenceBroker(t.TempDir())
			if !r.Ready {
				t.Errorf("%s must read ready, got %q", name, r.Detail)
			}
			if r.Remedy != "" {
				t.Errorf("a satisfied requirement must not nag: %q", r.Remedy)
			}
		})
	}
}
