package worktree

import (
	"os"
	"testing"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// TestMain pins the default OSBackend disk gate to a hermetic filesystem
// reading. This package's tests create real git worktrees under temp dirs
// through that gate, so they inherited whatever capacity the host reported —
// FAC-613: make ci at a8cd39e1 failed the whole package on a real WSL host
// (probe fails closed, physical drive below the 15 GiB reserve), and any
// macOS host under the 2% reserve fails identically (FAC-215 hermeticity).
// Gate behavior itself is covered by disk_gate_test.go's injected fakes, and
// production keeps the real host-volume bound and reserve policy.
func TestMain(m *testing.M) {
	restore := resources.SetOSBackendStatFSForTest(resources.HermeticStatFSForTest)
	code := m.Run()
	restore()
	os.Exit(code)
}
