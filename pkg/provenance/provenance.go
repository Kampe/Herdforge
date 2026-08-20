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
	errNotGitRoot   = errors.New("not a Git worktree")
)

type Info struct {
	Path           string
	SourceRevision string
	BinaryRevision string
	BuildTime      string
	SourceModule   string
	BinaryModule   string
	Comparable     bool
	Current        bool
}

// CurrentExecutable returns metadata embedded in the running executable.
// Unlike Read, it does not require the current directory to be a Git
// checkout, so callers such as `herd version` can report build identity from
// any working directory.
func CurrentExecutable() (Info, error) {
	path, err := os.Executable()
	if err != nil {
		return Info{}, fmt.Errorf("provenance: executable: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	revision, buildTime, module := binaryValuesFrom(path)
	return Info{Path: path, BinaryRevision: revision, BuildTime: buildTime, BinaryModule: module}, nil
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
	if err := verifyGitRoot(root); err != nil && !errors.Is(err, errNotGitRoot) {
		return Info{Path: path}, fmt.Errorf("provenance: %w", err)
	}
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return Info{Path: path}, fmt.Errorf("provenance: source revision: %w", err)
	}
	info := infoFor(path, source, root)
	info.Current = info.Comparable && Validate(info, source) == nil
	return info, nil
}

// ReadExecutable validates a selected executable path against root.
func ReadExecutable(path, root string) (Info, error) {
	if err := verifyGitRoot(root); err != nil {
		return Info{Path: path}, fmt.Errorf("provenance: %w", err)
	}
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return Info{Path: path}, err
	}
	info := infoFor(path, source, root)
	info.Current = info.Comparable && Validate(info, source) == nil
	return info, nil
}

// InstalledBinaryPaths is the complete set of consumer-visible herd paths.
// The root entry is intentionally a symlink to bin/herd, so callers can audit
// both names while the build only ever installs one executable inode.
func InstalledBinaryPaths(root string) []string {
	return []string{filepath.Join(root, "herd"), filepath.Join(root, "bin", "herd")}
}

// ValidateInstalled checks every installed consumer-visible path against the
// same source revision. A successful check can therefore never describe only
// whichever executable happened to launch the check.
func ValidateInstalled(root string) error {
	if err := verifyGitRoot(root); err != nil {
		return fmt.Errorf("provenance: %w", err)
	}
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return fmt.Errorf("provenance: source revision: %w", err)
	}
	infos := make([]Info, 0, len(InstalledBinaryPaths(root)))
	for _, path := range InstalledBinaryPaths(root) {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("installed herd binary %s: %w", path, err)
		}
		info := infoFor(path, source, root)
		if !info.Comparable {
			return fmt.Errorf("installed herd binary %s: source module %q does not match binary module %q", path, info.SourceModule, info.BinaryModule)
		}
		infos = append(infos, info)
	}
	return validateInstalledInfos(infos, source)
}

func validateInstalledInfos(infos []Info, source string) error {
	for _, info := range infos {
		if err := Validate(info, source); err != nil {
			return fmt.Errorf("%s: %w", info.Path, err)
		}
	}
	return nil
}

func infoFor(path, source, root string) Info {
	revision, buildTime, binaryModule := binaryValuesFrom(path)
	sourceModule := modulePath(root)
	comparable := sourceModule != "" && sourceModule == binaryModule
	return Info{
		Path:           path,
		SourceRevision: source,
		BinaryRevision: revision,
		BuildTime:      buildTime,
		SourceModule:   sourceModule,
		BinaryModule:   binaryModule,
		Comparable:     comparable,
	}
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
	if err := verifyGitRoot(root); err != nil {
		return Info{}, err
	}
	source, err := gitValue(root, "rev-parse", "HEAD")
	if err != nil {
		return Info{}, err
	}
	revision, buildTime, module := binaryValuesFrom(path)
	sourceModule := modulePath(root)
	return Info{Path: path, SourceRevision: source, BinaryRevision: revision, BuildTime: buildTime, SourceModule: sourceModule, BinaryModule: module, Comparable: sourceModule != "" && sourceModule == module}, nil
}

func verifyGitRoot(root string) error {
	want, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve source root: %w", err)
	}
	want, _ = filepath.EvalSymlinks(want)
	config := exec.Command("git", "config", "--local", "--get", "core.worktree")
	config.Dir = root
	if out, configErr := config.Output(); configErr == nil && strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("source Git core.worktree redirects to %q; refusing provenance", strings.TrimSpace(string(out)))
	}
	inside, err := gitValue(root, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return fmt.Errorf("%w: %v", errNotGitRoot, err)
	}
	if strings.TrimSpace(inside) != "true" {
		return errNotGitRoot
	}
	top, err := gitValue(root, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("source toplevel: %w", err)
	}
	top, _ = filepath.EvalSymlinks(top)
	if filepath.Clean(top) != filepath.Clean(want) {
		return fmt.Errorf("source Git toplevel mismatch: expected %q, got %q; refusing provenance", want, top)
	}
	return nil
}

func binaryValues() (string, string) {
	revision, buildTime, _ := binaryValuesFrom("")
	return revision, buildTime
}

func binaryValuesFrom(path string) (string, string, string) {
	revision, buildTime := strings.TrimSpace(BinaryRevision), strings.TrimSpace(BinaryBuildTime)
	module := ""
	var bi *debug.BuildInfo
	if path == "" {
		bi, _ = debug.ReadBuildInfo()
	} else {
		bi, _ = buildinfo.ReadFile(path)
	}
	if bi != nil {
		module = bi.Main.Path
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
	return revision, buildTime, module
}

func modulePath(root string) string {
	contents, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(contents), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "module" {
			return fields[1]
		}
	}
	return ""
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
	if !info.Comparable {
		current = "UNKNOWN"
	} else if info.Current {
		current = "CURRENT"
	}
	return fmt.Sprintf("herd provenance: %s\n  binary path: %s\n  source revision: %s\n  binary build revision: %s\n  binary build source: %s\n  source module: %s\n  binary build time: %s", current, info.Path, info.SourceRevision, info.BinaryRevision, info.BinaryModule, info.SourceModule, info.BuildTime)
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
