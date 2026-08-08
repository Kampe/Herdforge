package worktree

import (
	"context"
	"fmt"
	"strings"
)

// SafeRefPrefix is the durable refs namespace protecting a lane's working
// tip from destructive resets (FAC-214). Unlike the anchor ref (which
// protects the immutable base) and the salvage ref (which protects the tip
// before reap), the safe ref protects the lane's current HEAD from
// `git reset --hard origin/main` — the exact sequence that destroyed 5
// commits on FAC-172 and 36 commits on FAC-133 when lanes ran
// `git rebase --abort` followed by `git reset --hard origin/main`.
//
// The safe ref is a separate ref, not the branch, so a branch reset cannot
// make the captured commits unreachable. Recovery is always:
//
//	git reset --hard refs/herd/safe/<task>
//	# or cherry-pick individual commits:
//	git log origin/main..refs/herd/safe/<task>
const SafeRefPrefix = "refs/herd/safe/"

// SafeRefFor returns the durable safe ref for a task. The convention matches
// AnchorRefFor: lowercased task ref so macOS case-insensitive storage cannot
// alias FAC-172 and fac-172.
func SafeRefFor(taskRef string) string {
	return SafeRefPrefix + strings.ToLower(taskRef)
}

// WriteSafeRef writes or advances refs/herd/safe/<task> to headSHA and
// verifies the ref points at the expected commit before returning. The
// coordinator calls this before issuing any rebase instruction so the lane's
// current tip is captured independently of the branch ref — even if the lane
// subsequently runs `git reset --hard origin/main`, the commits remain
// reachable from the safe ref.
//
// headSHA must be a full 40-char SHA. An empty headSHA or taskRef fails
// closed.
func (w *WorktreeManager) WriteSafeRef(ctx context.Context, taskRef, headSHA string) error {
	if strings.TrimSpace(taskRef) == "" {
		return fmt.Errorf("safe ref: task ref is required")
	}
	if strings.TrimSpace(headSHA) == "" {
		return fmt.Errorf("safe ref: head SHA is required")
	}
	ref := SafeRefFor(taskRef)
	return w.writeSafeRef(ctx, ref, headSHA)
}

func (w *WorktreeManager) writeSafeRef(ctx context.Context, ref, sha string) error {
	if err := w.updateRef(ctx, ref, sha); err != nil {
		return fmt.Errorf("safe ref %s: %w", ref, err)
	}
	got, err := w.revParse(ctx, ref)
	if err != nil {
		return fmt.Errorf("safe ref %s verify read: %w", ref, err)
	}
	if got != sha {
		return fmt.Errorf("safe ref %s verification failed: got %q want %q", ref, got, sha)
	}
	return nil
}

// ReadSafeRef returns the SHA that refs/herd/safe/<task> points at. An empty
// string with a nil error means the safe ref does not exist yet (the lane
// was created before FAC-214 or the write failed silently at creation).
func (w *WorktreeManager) ReadSafeRef(ctx context.Context, taskRef string) (string, error) {
	if strings.TrimSpace(taskRef) == "" {
		return "", fmt.Errorf("safe ref: task ref is required")
	}
	ref := SafeRefFor(taskRef)
	sha, err := w.revParse(ctx, ref)
	if err != nil {
		return "", nil
	}
	return sha, nil
}

// DropReport is the result of DetectDroppedWork. Dropped is true only when
// the lane's HEAD has fallen back to origin/main while the safe ref still
// holds a divergent tip — the signature of a `git reset --hard origin/main`
// that discarded commits not on any remote.
type DropReport struct {
	TaskRef        string   `json:"task_ref"`
	SafeRef        string   `json:"safe_ref"`
	SafeRefSHA     string   `json:"safe_ref_sha"`
	HeadSHA        string   `json:"head_sha"`
	OriginMainSHA  string   `json:"origin_main_sha"`
	Dropped        bool     `json:"dropped"`
	Recoverable    bool     `json:"recoverable"`
	UniqueSubjects []string `json:"unique_subjects,omitempty"`
}

// DetectDroppedWork checks whether a lane's HEAD has been destructively
// reset to origin/main while the safe ref still captures divergent commits.
// This is the "immediately detectable" half of FAC-214: the coordinator
// calls this when inspecting lane health, and a Dropped=true report is an
// alarm that commits were lost from the branch but are still reachable from
// the safe ref.
//
// Detection logic:
//   - No safe ref → Dropped=false (no baseline; pre-FAC-214 lane).
//   - headSHA != originMainSHA → Dropped=false (lane still holds its work).
//   - headSHA == originMainSHA && safeRefSHA == originMainSHA → Dropped=false
//     (lane was at origin/main; nothing was lost).
//   - headSHA == originMainSHA && safeRefSHA != originMainSHA → Dropped=true.
//     UniqueSubjects lists commits on the safe ref not on origin/main
//     (excluding the FAC-106 anchor commit, which is infrastructure).
//
// Recoverable is true when Dropped is true and the safe ref SHA is still
// resolvable (the commits have not been garbage-collected).
func (w *WorktreeManager) DetectDroppedWork(ctx context.Context, taskRef, headSHA, originMainSHA string) (*DropReport, error) {
	if strings.TrimSpace(taskRef) == "" {
		return nil, fmt.Errorf("detect dropped work: task ref is required")
	}
	ref := SafeRefFor(taskRef)
	safeSHA, _ := w.ReadSafeRef(ctx, taskRef)
	report := &DropReport{
		TaskRef:       taskRef,
		SafeRef:       ref,
		SafeRefSHA:    safeSHA,
		HeadSHA:       headSHA,
		OriginMainSHA: originMainSHA,
	}
	if safeSHA == "" {
		return report, nil
	}
	if headSHA != originMainSHA {
		return report, nil
	}
	if safeSHA == originMainSHA {
		return report, nil
	}
	report.Dropped = true
	if _, err := w.revParse(ctx, ref); err == nil {
		report.Recoverable = true
	}
	report.UniqueSubjects = w.safeRefUniqueSubjects(ctx, originMainSHA, safeSHA)
	return report, nil
}

// safeRefUniqueSubjects returns commit subjects on the safe ref that are not
// on origin/main, excluding the FAC-106 anchor commit (infrastructure, not
// real work). Errors are swallowed: the caller already has Dropped=true and
// can act on the safe ref even without subject detail.
func (w *WorktreeManager) safeRefUniqueSubjects(ctx context.Context, originMainSHA, safeSHA string) []string {
	cmd := execCommandContext(ctx, "git", "log", "--format=%s", originMainSHA+".."+safeSHA)
	cmd.Dir = w.RepoRoot
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var subjects []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.Contains(line, "anchor") && strings.Contains(line, "reap-safe") {
			continue
		}
		subjects = append(subjects, line)
	}
	return subjects
}
