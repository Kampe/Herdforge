package worktree

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// ExpiryPolicy bounds the lossless orphan collector. Processes is mandatory:
// an unavailable owner census is not evidence that an old checkout is unused.
type ExpiryPolicy struct {
	RepoRoot      string
	StateRoot     string
	InactiveFor   time.Duration
	Processes     resources.ProcessInspector
	Now           func() time.Time
	CommitMessage string
}

type ExpiryResult struct {
	Kind       string
	ID         string
	ArchiveRef string
	Archived   bool
	Skipped    string
}

// ArchiveExpired preserves a stale, ownerless worktree under an archive ref
// before removing its checkout. Dirty files are committed before archiving.
func ArchiveExpired(ctx context.Context, policy ExpiryPolicy) ([]ExpiryResult, error) {
	if strings.TrimSpace(policy.RepoRoot) == "" || strings.TrimSpace(policy.StateRoot) == "" {
		return nil, fmt.Errorf("expiry requires repository and state roots")
	}
	if policy.InactiveFor <= 0 {
		return nil, fmt.Errorf("expiry inactivity period must be positive")
	}
	if policy.Processes == nil {
		return nil, fmt.Errorf("expiry requires a process census")
	}
	if policy.Now == nil {
		policy.Now = time.Now
	}
	if policy.CommitMessage == "" {
		policy.CommitMessage = "herd archive: preserve expired worktree"
	}
	records, err := readActiveLifecycleRecords(policy.StateRoot)
	if err != nil {
		return nil, err
	}
	now := policy.Now().UTC()
	results := make([]ExpiryResult, 0, len(records))
	for _, item := range records {
		record := item.Record
		result := ExpiryResult{Kind: record.Kind, ID: record.Task}
		if record.Repository != RepositoryID(policy.RepoRoot) {
			result.Skipped = "repository identity differs"
			results = append(results, result)
			continue
		}
		path, err := lifecycleCheckoutPath(policy.StateRoot, record)
		if err != nil {
			return results, err
		}
		activity := record.LastActivityAt
		if activity.IsZero() {
			activity = record.CreatedAt
		}
		if activity.IsZero() || now.Sub(activity) < policy.InactiveFor {
			result.Skipped = "activity is within expiry period"
			results = append(results, result)
			continue
		}
		usage, err := policy.Processes.InUse(ctx, path)
		if err != nil {
			return results, fmt.Errorf("expiry owner census for %s/%s: %w", record.Kind, record.Task, err)
		}
		if usage.MetadataUnavailable {
			result.Skipped = "owner census incomplete"
			results = append(results, result)
			continue
		}
		if usage.CWD || usage.OpenFile || usage.ReferencedPath || len(usage.PIDs) > 0 {
			result.Skipped = "live owner holds checkout"
			results = append(results, result)
			continue
		}
		if err := archiveAndRemove(ctx, policy, item.Path, path, record); err != nil {
			return results, err
		}
		result.Archived, result.ArchiveRef = true, archiveRef(record.Kind, record.Task)
		results = append(results, result)
	}
	return results, nil
}

type lifecycleRecordFile struct {
	Path   string
	Record LifecycleRecord
}

func readActiveLifecycleRecords(stateRoot string) ([]lifecycleRecordFile, error) {
	root := filepath.Join(stateRoot, "records")
	entries := []lifecycleRecordFile{}
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || filepath.Ext(path) != ".json" {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var record LifecycleRecord
		if err := json.Unmarshal(data, &record); err != nil {
			return fmt.Errorf("decode lifecycle record %s: %w", filepath.Base(path), err)
		}
		if record.State == "active" {
			entries = append(entries, lifecycleRecordFile{Path: path, Record: record})
		}
		return nil
	})
	if os.IsNotExist(err) {
		return entries, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read lifecycle records: %w", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

func lifecycleCheckoutPath(stateRoot string, record LifecycleRecord) (string, error) {
	if safeLifecyclePart(record.Kind) == "" || safeLifecyclePart(record.Task) == "" {
		return "", fmt.Errorf("expiry record has no portable kind/id")
	}
	if filepath.IsAbs(record.Path) || strings.TrimSpace(record.Path) == "" {
		return "", fmt.Errorf("expiry record %s/%s has non-portable path", record.Kind, record.Task)
	}
	path := filepath.Join(stateRoot, filepath.FromSlash(record.Path))
	rel, err := filepath.Rel(stateRoot, path)
	if err != nil || rel == "." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("expiry record %s/%s escaped state root", record.Kind, record.Task)
	}
	return path, nil
}

func archiveRef(kind, id string) string {
	return "refs/herd/archive/" + safeLifecyclePart(kind) + "/" + safeLifecyclePart(id)
}

func archiveAndRemove(ctx context.Context, policy ExpiryPolicy, recordFile, path string, record LifecycleRecord) error {
	status, err := gitAt(ctx, policy.RepoRoot, path, "status", "--porcelain")
	if err != nil {
		return fmt.Errorf("expiry status %s/%s: %w", record.Kind, record.Task, err)
	}
	if strings.TrimSpace(status) != "" {
		if _, err := gitAt(ctx, policy.RepoRoot, path, "add", "-A"); err != nil {
			return fmt.Errorf("archive dirty changes %s/%s: %w", record.Kind, record.Task, err)
		}
		if _, err := gitAt(ctx, policy.RepoRoot, path, "-c", "user.name=herdforge-archive", "-c", "user.email=herdforge-archive@invalid", "commit", "-m", policy.CommitMessage); err != nil {
			return fmt.Errorf("commit archived changes %s/%s: %w", record.Kind, record.Task, err)
		}
	}
	head, err := gitAt(ctx, policy.RepoRoot, path, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("read archive head %s/%s: %w", record.Kind, record.Task, err)
	}
	if _, err := gitAt(ctx, policy.RepoRoot, policy.RepoRoot, "update-ref", archiveRef(record.Kind, record.Task), strings.TrimSpace(head)); err != nil {
		return fmt.Errorf("write archive ref %s/%s: %w", record.Kind, record.Task, err)
	}
	if _, err := gitAt(ctx, policy.RepoRoot, policy.RepoRoot, "worktree", "remove", path); err != nil {
		return fmt.Errorf("remove archived worktree %s/%s: %w", record.Kind, record.Task, err)
	}
	record.State, record.RetiredAt = "retired", policy.Now().UTC()
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal retired lifecycle record: %w", err)
	}
	if err := os.WriteFile(recordFile, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write retired lifecycle record: %w", err)
	}
	return nil
}

func gitAt(ctx context.Context, repoRoot, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}
