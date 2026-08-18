package preflight

import (
	"fmt"
	"strconv"
	"strings"
)

// MainOriginDivergence describes commits present on one main ref but not the
// other. LocalAhead is the number of commits in main but not origin/main;
// RemoteAhead is the number of commits in origin/main but not main.
type MainOriginDivergence struct {
	LocalAhead  int
	RemoteAhead int
}

// CheckMainOriginDivergence fails closed when the local main and origin/main
// refs are missing or have different histories. It reads refs only: preflight
// must report drift without fetching or changing the repository.
func CheckMainOriginDivergence(root string) (MainOriginDivergence, error) {
	out, err := runCmd(root, "git", "rev-list", "--left-right", "--count", "main...origin/main")
	if err != nil {
		return MainOriginDivergence{}, fmt.Errorf("check main/origin/main divergence: %w", err)
	}
	parts := strings.Fields(out)
	if len(parts) != 2 {
		return MainOriginDivergence{}, fmt.Errorf("check main/origin/main divergence: expected two counts, got %q", out)
	}
	localAhead, err := strconv.Atoi(parts[0])
	if err != nil {
		return MainOriginDivergence{}, fmt.Errorf("check main/origin/main divergence: parse local count %q: %w", parts[0], err)
	}
	remoteAhead, err := strconv.Atoi(parts[1])
	if err != nil {
		return MainOriginDivergence{}, fmt.Errorf("check main/origin/main divergence: parse origin count %q: %w", parts[1], err)
	}
	report := MainOriginDivergence{LocalAhead: localAhead, RemoteAhead: remoteAhead}
	if report.LocalAhead != 0 || report.RemoteAhead != 0 {
		return report, fmt.Errorf("main/origin/main diverged: main is %d commit(s) ahead and origin/main is %d commit(s) ahead", report.LocalAhead, report.RemoteAhead)
	}
	return report, nil
}
