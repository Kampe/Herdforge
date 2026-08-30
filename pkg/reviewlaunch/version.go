package reviewlaunch

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var herdVersionRevisionRE = regexp.MustCompile(`\brevision\s+([^,\s)]+)`)

// VersionRequirement is the binary contract a remote review host must satisfy
// before a launcher depends on a herd subcommand there.
type VersionRequirement struct {
	RequiredCommand string
	LocalRevision   string
	RemoteRevision  string
}

// VersionDriftError identifies a remote herd that cannot satisfy the launcher's
// command contract. Keep the three fields in every diagnostic: operators need
// to know what is missing and which binary must be replaced.
type VersionDriftError struct {
	Requirement VersionRequirement
	Cause       string
}

func (e *VersionDriftError) Error() string {
	return fmt.Sprintf("remote herd version drift: required subcommand=%q local_revision=%s remote_revision=%s: %s",
		e.Requirement.RequiredCommand, revisionOrUnknown(e.Requirement.LocalRevision),
		revisionOrUnknown(e.Requirement.RemoteRevision), e.Cause)
}

// ParseHerdVersion extracts the revision from `herd --version` output.
func ParseHerdVersion(output string) (string, error) {
	match := herdVersionRevisionRE.FindStringSubmatch(output)
	if len(match) != 2 || strings.TrimSpace(match[1]) == "" || match[1] == "unknown" {
		return "", fmt.Errorf("herd version output has no revision: %q", strings.TrimSpace(output))
	}
	return strings.TrimSpace(match[1]), nil
}

// RequireRemoteHerd rejects revision mismatch before a launcher invokes the
// required command. If the revisions appear equal but the command reports
// "unknown command", classify that contradiction as drift rather than a
// generic launcher failure.
func RequireRemoteHerd(req VersionRequirement, commandOutput string, commandErr error) error {
	req.RequiredCommand = strings.TrimSpace(req.RequiredCommand)
	req.LocalRevision = strings.TrimSpace(req.LocalRevision)
	req.RemoteRevision = strings.TrimSpace(req.RemoteRevision)
	if req.RequiredCommand == "" {
		return errors.New("reviewlaunch: required remote herd subcommand is empty")
	}
	if req.LocalRevision == "" || req.RemoteRevision == "" || req.LocalRevision != req.RemoteRevision {
		return &VersionDriftError{Requirement: req, Cause: "launcher and remote herd revisions differ"}
	}
	if commandErr == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(commandOutput), "unknown command") {
		return &VersionDriftError{Requirement: req, Cause: fmt.Sprintf("remote herd does not provide %q", req.RequiredCommand)}
	}
	return fmt.Errorf("reviewlaunch: probe remote herd subcommand %q at revision %s: %w: %s",
		req.RequiredCommand, req.RemoteRevision, commandErr, strings.TrimSpace(commandOutput))
}

func revisionOrUnknown(revision string) string {
	revision = strings.TrimSpace(revision)
	if revision == "" {
		return "unknown"
	}
	return revision
}
