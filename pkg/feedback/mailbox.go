package feedback

import (
	"os"
	"path/filepath"
	"strings"
)

// FleetMailDir resolves the directory this package writes per-recipient feedback
// mailboxes into.
//
// FAC-629: exported so `herd mail inbox` can DRAIN what this package writes.
// Previously the write path lived here and no reader existed anywhere, so a lane
// polled for feedback was told it had "no mailbox history" while its requests sat
// on disk. Exported rather than re-derived in cmd/, because two copies of a path
// rule drift and only one of them gets fixed -- the exact failure FAC-613 removed
// from this repo.
func FleetMailDir(repoRoot string) string {
	if dir := strings.TrimSpace(os.Getenv(EnvMailDir)); dir != "" {
		return dir
	}
	return filepath.Join(defaultFleetStateDir(repoRoot), "mail")
}
