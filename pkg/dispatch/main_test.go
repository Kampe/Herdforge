package dispatch

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain sets HERD_LAUNCH_RECEIPTS to a temp file so launch.reject() and
// launch.RecordStarted() — which fall back to DefaultSink() (relative
// .herd/launch-receipts.jsonl) when no sink is provided — do not pollute
// the package directory. Individual tests that need a specific receipt path
// override this via t.Setenv.
func TestMain(m *testing.M) {
	if os.Getenv("HERD_LAUNCH_RECEIPTS") == "" {
		os.Setenv("HERD_LAUNCH_RECEIPTS", filepath.Join(os.TempDir(), "herd-dispatch-test-receipts.jsonl"))
	}
	os.Exit(m.Run())
}
