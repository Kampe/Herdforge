// Package provenance identifies the Herdforge source and executable used by
// native self-gates. A PATH entry is not evidence that it was built from the
// source being checked.
package provenance

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"
)

// These are populated by release/local build tooling. The runtime build info
// fallback keeps ordinary `go build` and `go run` useful without requiring
// callers to know the ldflags.
var (
	BinaryRevision  = ""
	BinaryBuildTime = ""
)

type Info struct {
	Path           string
	SourceRevision string
	BinaryRevision string
	BuildTime      string
	Current        bool
}

// Read returns the executable and source identity for root.
func Read(root string) (Info, error) {
	path, err := os.Executable()
	if err != nil {
		return Info{}, fmt.Errorf("provenance: executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return Info{Path: path}, fmt.Errorf("provenance: source revision: %w", err)
	}
	info := infoFor(path, source)
	info.Current = Validate(info, source) == nil
	return info, nil
}

// ReadExecutable validates a selected executable path against root.
func ReadExecutable(path, root string) (Info, error) {
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return Info{Path: path}, err
	}
	info := infoFor(path, source)
	info.Current = Validate(info, source) == nil
	return info, nil
}

func infoFor(path, source string) Info {
	revision, buildTime := binaryValuesFrom(path)
	return Info{Path: path, SourceRevision: source, BinaryRevision: revision, BuildTime: buildTime}
}

// Validate rejects a binary that cannot be tied to the checked source.
func Validate(info Info, source string) error {
	source = strings.TrimSpace(source)
	if source == "" {
		return errors.New("source revision is empty")
	}
	if strings.TrimSpace(info.BinaryRevision) != "" {
		if info.BinaryRevision != source {
			return fmt.Errorf("stale herd binary: source revision=%s, binary revision=%s", short(source), short(info.BinaryRevision))
		}
		return nil
	}
	return errors.New("herd binary has no source revision metadata")
}

// Resolve returns a current herd executable for root. PATH remains convenient
// for local installs, but an older PATH entry is never selected silently.
func Resolve(root string) (string, error) {
	candidates := []string{}
	if explicit := strings.TrimSpace(os.Getenv("HERD_BIN")); explicit != "" {
		candidates = append(candidates, explicit)
	}
	if path, err := exec.LookPath("herd"); err == nil {
		candidates = append(candidates, path)
	}
	if root != "" {
		candidates = append(candidates, filepath.Join(root, "bin", "herd"))
	}
	var reasons []string
	seen := map[string]bool{}
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil || seen[abs] {
			continue
		}
		seen[abs] = true
		st, err := os.Stat(abs)
		if err != nil || st.IsDir() {
			continue
		}
		info, err := executableInfo(abs, root)
		if err == nil {
			if err = Validate(info, info.SourceRevision); err == nil {
				return abs, nil
			}
		}
		reasons = append(reasons, fmt.Sprintf("%s: %v", candidate, err))
	}
	if len(reasons) == 0 {
		return "", errors.New("herd: no executable found (build ./cmd/herd or add herd to PATH)")
	}
	return "", fmt.Errorf("herd: no current executable found: %s", strings.Join(reasons, "; "))
}

func executableInfo(path, root string) (Info, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return Info{}, err
	}
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return Info{}, err
	}
	revision, buildTime := binaryValuesFrom(path)
	return Info{Path: path, SourceRevision: source, BinaryRevision: revision, BuildTime: buildTime}, nil
}

func binaryValues() (string, string) { return binaryValuesFrom("") }

func binaryValuesFrom(path string) (string, string) {
	revision, buildTime := strings.TrimSpace(BinaryRevision), strings.TrimSpace(BinaryBuildTime)
	var bi *debug.BuildInfo
	if path == "" {
		bi, _ = debug.ReadBuildInfo()
	} else {
		bi, _ = buildinfo.ReadFile(path)
	}
	if bi != nil {
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if revision == "" {
					revision = setting.Value
				}
			case "vcs.time":
				if buildTime == "" {
					buildTime = setting.Value
				}
			}
		}
	}
	return revision, buildTime
}

func gitValue(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func short(value string) string {
	if len(value) > 12 {
		return value[:12]
	}
	return value
}

// Format produces stable, human-readable self-gate evidence.
func Format(info Info) string {
	current := "STALE"
	if info.Current {
		current = "CURRENT"
	}
	return fmt.Sprintf("herd provenance: %s\n  binary path: %s\n  source revision: %s\n  binary build revision: %s\n  binary build time: %s", current, info.Path, info.SourceRevision, info.BinaryRevision, info.BuildTime)
}

// ParseUnixTime is kept small and exported for table tests and future gate
// reporting; it accepts the timestamp format emitted by Go build metadata.
func ParseUnixTime(value string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.Unix(seconds, 0).UTC()
}
