package reviewingest

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// IngestedAuditFinding identifies an artifact under an ingested directory
// that has no matching durable verdict event.
type IngestedAuditFinding struct {
	Path   string `json:"path"`
	SHA    string `json:"sha,omitempty"`
	Reason string `json:"reason"`
}

// AuditIngested walks ingested directories directly. It deliberately does not
// derive its candidate set from record events: an artifact with no record is
// itself an audit finding.
func AuditIngested(root string, rows []reviewledger.LedgerRow) ([]IngestedAuditFinding, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("audit root is required")
	}
	verdicts := make(map[string]struct{})
	for _, row := range rows {
		if row.Event == string(reviewledger.EventVerdict) && strings.TrimSpace(row.SHA) != "" {
			verdicts[row.SHA] = struct{}{}
		}
	}
	var findings []IngestedAuditFinding
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || filepath.Base(filepath.Dir(path)) != "ingested" {
			return nil
		}
		body, readErr := os.ReadFile(path) // #nosec G122 -- the artifact path is supplied by the bounded WalkDir rooted at the configured review store.
		if readErr != nil {
			findings = append(findings, IngestedAuditFinding{Path: filepath.ToSlash(path), Reason: fmt.Sprintf("read artifact: %v", readErr)})
			return nil
		}
		a := Parse(string(body))
		if _, ok := verdicts[a.SHA]; !ok {
			reason := "no matching verdict event"
			if strings.TrimSpace(a.SHA) == "" {
				reason = "artifact has no candidate sha"
			}
			findings = append(findings, IngestedAuditFinding{Path: filepath.ToSlash(path), SHA: a.SHA, Reason: reason})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk ingested artifacts: %w", err)
	}
	return findings, nil
}

// MoveToIngested moves an artifact only after its caller has proved ledger
// admission. Existing same-content destinations are treated idempotently.
func MoveToIngested(source string) (string, error) {
	return MoveToIngestedNamed(source, filepath.Base(source))
}

// MoveToIngestedNamed moves an artifact to ingested under the supplied
// basename. The destination is checked before the rename, and a different
// existing artifact is always refused without consuming the source.
func MoveToIngestedNamed(source, name string) (string, error) {
	source = strings.TrimSpace(source)
	name = strings.TrimSpace(name)
	if source == "" || name == "" || filepath.Base(name) != name {
		return "", fmt.Errorf("artifact source is required")
	}
	if filepath.Base(filepath.Dir(source)) == "ingested" {
		return source, nil
	}
	dst := filepath.Join(filepath.Dir(source), "ingested", name)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("create ingested directory: %w", err)
	}
	if existing, err := os.ReadFile(dst); err == nil {
		incoming, readErr := os.ReadFile(source)
		if readErr != nil {
			return "", fmt.Errorf("read artifact before idempotent move: %w", readErr)
		}
		if string(existing) != string(incoming) {
			return "", fmt.Errorf("ingested artifact %s exists with different content", dst)
		}
		return dst, nil
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("check ingested artifact: %w", err)
	}
	// Link publishes without replacing a destination that may appear after the
	// preflight check. Remove the source only after publication succeeds.
	if err := os.Link(source, dst); err != nil {
		return "", fmt.Errorf("move artifact into ingested: %w", err)
	}
	if err := os.Remove(source); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("remove source after move: %w", err)
	}
	return dst, nil
}

// CheckMoveToIngested verifies the destination without moving the source.
// Dry-run uses the same collision check as the real move.
func CheckMoveToIngested(source, name string) error {
	source = strings.TrimSpace(source)
	name = strings.TrimSpace(name)
	if source == "" || name == "" || filepath.Base(name) != name {
		return fmt.Errorf("artifact source and destination name are required")
	}
	dst := filepath.Join(filepath.Dir(source), "ingested", name)
	existing, err := os.ReadFile(dst)
	if err == nil {
		incoming, readErr := os.ReadFile(source)
		if readErr != nil {
			return fmt.Errorf("read artifact before idempotent move: %w", readErr)
		}
		if string(existing) != string(incoming) {
			return fmt.Errorf("ingested artifact %s exists with different content", dst)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("check ingested artifact: %w", err)
	}
	return nil
}

// InboxRel is the repository-relative durable review inbox. Reviewer panes
// write ephemeral temp-path artifacts; only copies under this directory are
// review authority after pane cleanup (FAC-373).
const InboxRel = ".herd/review/inbox"

// RetainArtifact copies a validated verdict into the repo-local review inbox
// using a content-addressed filename. The returned path is relative to root
// (slash-separated) so the ledger remains portable across worktrees.
//
// Callers must retain before treating a PASS as cleanup-ready: ephemeral
// /tmp or chainseer-herd-review paths may vanish the moment the reviewer
// pane exits.
func RetainArtifact(root, source, sha, reviewer string) (string, error) {
	root = strings.TrimSpace(root)
	source = strings.TrimSpace(source)
	sha = strings.TrimSpace(sha)
	if root == "" || source == "" || sha == "" {
		return "", fmt.Errorf("root, source, and candidate sha are required")
	}
	in, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open verdict artifact: %w", err)
	}
	defer in.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, in); err != nil {
		return "", fmt.Errorf("hash verdict artifact: %w", err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		return "", fmt.Errorf("rewind verdict artifact: %w", err)
	}
	contentDigest := fmt.Sprintf("%x", digest.Sum(nil))[:16]
	name := fmt.Sprintf("%s-%s-%s.md", strings.ToLower(shortSHA(sha)), sanitizeReviewerName(reviewer), contentDigest)
	rel := filepath.ToSlash(filepath.Join(InboxRel, name))
	dst := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return "", fmt.Errorf("create review inbox: %w", err)
	}
	// Idempotent same-content hit: a prior retain already published this
	// digest for the exact candidate. Do not rewrite review evidence.
	if existing, err := os.ReadFile(dst); err == nil {
		sum := sha256.Sum256(existing)
		if fmt.Sprintf("%x", sum)[:16] == contentDigest {
			if _, err := os.Stat(dst); err != nil {
				return "", fmt.Errorf("retained artifact vanished after read: %w", err)
			}
			return rel, nil
		}
		return "", fmt.Errorf("retained artifact %s exists with different content", rel)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("stat retained artifact: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), ".verdict-*.tmp")
	if err != nil {
		return "", fmt.Errorf("create retained artifact: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := io.Copy(tmp, in); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("copy verdict artifact: %w", err)
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("secure retained artifact: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("sync retained artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("close retained artifact: %w", err)
	}
	if err := os.Rename(tmpName, dst); err != nil {
		return "", fmt.Errorf("publish retained artifact: %w", err)
	}
	// Re-stat after publish: a concurrent cleanup of the destination (or a
	// flaky FS) must never become coordinator-facing PASS evidence.
	info, err := os.Stat(dst)
	if err != nil {
		return "", fmt.Errorf("retained artifact vanished after publish: %w", err)
	}
	if info.Size() == 0 {
		return "", fmt.Errorf("retained artifact is empty after publish")
	}
	return rel, nil
}

// RetainedArtifactName returns the reviewer-qualified basename used for a
// retained artifact and its matching ingested handoff.
func RetainedArtifactName(sha, reviewer string, body []byte) string {
	digest := sha256.Sum256(body)
	return fmt.Sprintf("%s-%s-%x.md", strings.ToLower(shortSHA(strings.TrimSpace(sha))), sanitizeReviewerName(reviewer), digest[:8])
}

// IsInboxPath reports whether path is a repository-relative durable review
// inbox artifact (slash or OS separators accepted).
func IsInboxPath(path string) bool {
	p := filepath.ToSlash(strings.TrimSpace(path))
	return strings.HasPrefix(p, InboxRel+"/") || p == InboxRel
}

func sanitizeReviewerName(reviewer string) string {
	name := strings.ToLower(strings.TrimSpace(reviewer))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	name = strings.Trim(b.String(), "-")
	if name == "" {
		name = "reviewer"
	}
	if len(name) > 32 {
		name = name[:32]
	}
	return name
}
