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
	// Dispatch tests exercise receipt validation and launch orchestration, not
	// the coordinator boundary of the ambient worker process. Keep the suite
	// outside managed worktrees and clear Herdr's inherited agent marker.
	testCWD, err := os.MkdirTemp("", "herd-dispatch-tests-")
	if err != nil || os.Chdir(testCWD) != nil {
		os.Exit(1)
	}
	os.Unsetenv("HERD_ROLE")
	code := m.Run()
	_ = os.RemoveAll(testCWD)
	os.Exit(code)
}
