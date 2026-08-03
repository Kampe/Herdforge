package harvest

// This file is the compiled replay authority. It is deliberately separate
// from Integration: replay must never become a shell-driven merge/push loop.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

const replayStateVersion = 1

type ReplayClass string

const (
	ReplayAppliedExact   ReplayClass = "applied_exact_patch"
	ReplayAlreadyPresent ReplayClass = "already_present_stable_patch"
	ReplayEmptyAnchor    ReplayClass = "intentionally_empty_anchor"
	ReplayUnresolved     ReplayClass = "unresolved"
)

var (
	ErrReplayBlocked = errors.New("serialized replay blocked")
	ErrStaleReplay   = errors.New("serialized replay stale destination or generation")
)

type ReplayRequest struct {
	RepoRoot      string
	TaskID        string
	RepoID        string
	ExpectedHead  string
	Generation    string
	SourceCommits []string
	EvidencePath  string
	RecoveryPath  string
	sourceDigest  string
}

type ReplayItem struct {
	Order           int         `json:"order"`
	Source          string      `json:"source"`
	PatchID         string      `json:"patch_id,omitempty"`
	Classification  ReplayClass `json:"classification"`
	Matched         string      `json:"matched,omitempty"`
	BaseHead        string      `json:"base_head"`
	DestinationHead string      `json:"destination_head"`
}

type ReplayResult struct {
	Generation   string       `json:"generation"`
	ExpectedHead string       `json:"expected_head"`
	FinalHead    string       `json:"final_head"`
	Items        []ReplayItem `json:"items"`
	Completed    bool         `json:"completed"`
}

type blockedEvidence struct {
	Timestamp        string   `json:"timestamp"`
	TaskID           string   `json:"task_id"`
	RepoID           string   `json:"repo_id"`
	SourceDigest     string   `json:"source_digest"`
	Generation       string   `json:"generation"`
	ExpectedHead     string   `json:"expected_head"`
	ActualHead       string   `json:"actual_head,omitempty"`
	Source           string   `json:"source,omitempty"`
	Reason           string   `json:"reason_code"`
	DiagnosticDigest string   `json:"diagnostic_digest"`
	Sequencer        []string `json:"sequencer_evidence,omitempty"`
	Recovery         string   `json:"recovery_evidence,omitempty"`
}

type replayState struct {
	Version      int          `json:"version"`
	Generation   string       `json:"generation"`
	TaskID       string       `json:"task_id"`
	RepoID       string       `json:"repo_id"`
	ExpectedHead string       `json:"expected_head"`
	SourceDigest string       `json:"source_digest"`
	Sources      []string     `json:"sources"`
	CurrentHead  string       `json:"current_head"`
	Items        []ReplayItem `json:"items"`
	Completed    bool         `json:"completed"`
}

// Replay executes the ordered source list. It never invokes a shell and never
// aborts a failed cherry-pick: CHERRY_PICK_HEAD and .git/sequencer are part of
// the evidence needed for an operator/recovery agent to continue safely.
func Replay(ctx context.Context, req ReplayRequest) (result *ReplayResult, retErr error) {
	if err := validateReplayRequest(req); err != nil {
		return nil, err
	}
	gitDir, err := replayGitDir(ctx, req.RepoRoot)
	if err != nil {
		return nil, err
	}
	commonDir, err := replayCommonDir(ctx, req.RepoRoot)
	if err != nil {
		return nil, err
	}
	rawDigest := digestSources(req.SourceCommits)
	statePath, evidencePath, err := replayArtifactPaths(req, rawDigest)
	if err != nil {
		return nil, err
	}
	if err := secureRepoPath(req.RepoRoot, statePath); err != nil {
		return nil, err
	}
	if err := secureRepoPath(req.RepoRoot, evidencePath); err != nil {
		return nil, err
	}
	unlock, err := acquireReplayLock(commonDir)
	if err != nil {
		return nil, blockAt(req, evidencePath, gitDir, err.Error(), "", "")
	}
	defer func() {
		if unlockErr := unlock(); unlockErr != nil {
			blocked := blockAt(req, evidencePath, gitDir, "replay unlock failed: "+unlockErr.Error(), "", "")
			if retErr == nil {
				retErr = blocked
			} else {
				retErr = errors.Join(retErr, blocked)
			}
		}
	}()
	if err := validateExpectedHead(ctx, req.RepoRoot, req.ExpectedHead); err != nil {
		return nil, blockAt(req, evidencePath, gitDir, err.Error(), "", "")
	}
	sources, err := resolveSources(ctx, req.RepoRoot, req.SourceCommits)
	if err != nil {
		return nil, blockAt(req, evidencePath, gitDir, err.Error(), "", "")
	}
	sourcesDigest := digestSources(sources)
	req.sourceDigest = sourcesDigest
	statePath, evidencePath, err = replayArtifactPaths(req, sourcesDigest)
	if err != nil {
		return nil, err
	}
	if err := secureRepoPath(req.RepoRoot, statePath); err != nil {
		return nil, err
	}
	if err := secureRepoPath(req.RepoRoot, evidencePath); err != nil {
		return nil, err
	}

	req.sourceDigest = sourcesDigest
	recoveryRef := replayRecoveryRef(req, sourcesDigest)
	state, err := loadReplayState(req.RepoRoot, statePath)
	if err != nil {
		return nil, err
	}
	if state != nil {
		if err := validateReplayState(ctx, req.RepoRoot, state, req, sources, sourcesDigest); err != nil {
			return nil, blockAt(req, evidencePath, gitDir, err.Error(), "", "")
		}
	} else {
		head, e := gitText(ctx, req.RepoRoot, "rev-parse", "HEAD")
		if e != nil {
			return nil, e
		}
		if strings.TrimSpace(head) != req.ExpectedHead {
			return nil, blockAt(req, evidencePath, gitDir, "destination head is stale before replay", strings.TrimSpace(head), "")
		}
		zero, err := zeroObjectID(ctx, req.RepoRoot)
		if err != nil {
			return nil, blockAt(req, evidencePath, gitDir, err.Error(), strings.TrimSpace(head), "")
		}
		if err := updateRecoveryRef(ctx, req.RepoRoot, recoveryRef, zero, req.ExpectedHead); err != nil {
			return nil, blockAt(req, evidencePath, gitDir, "recovery ref ownership CAS failed: "+err.Error(), strings.TrimSpace(head), "")
		}
		state = &replayState{Version: replayStateVersion, Generation: req.Generation, TaskID: req.TaskID, RepoID: req.RepoID, ExpectedHead: req.ExpectedHead, SourceDigest: sourcesDigest, Sources: sources, CurrentHead: req.ExpectedHead}
		if err := saveReplayState(req.RepoRoot, statePath, state); err != nil {
			rollbackErr := updateRecoveryRef(ctx, req.RepoRoot, recoveryRef, req.ExpectedHead, zero)
			reason := "initial replay state persistence failed: " + err.Error()
			if rollbackErr != nil {
				reason += "; recovery-ref rollback failed: " + rollbackErr.Error()
			}
			return nil, blockAt(req, evidencePath, gitDir, reason, strings.TrimSpace(head), "")
		}
	}

	actualHead, err := gitText(ctx, req.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(actualHead) != state.CurrentHead {
		return nil, blockAt(req, evidencePath, gitDir, "destination head changed during replay restart", strings.TrimSpace(actualHead), "")
	}
	refHead, err := gitText(ctx, req.RepoRoot, "rev-parse", "--verify", recoveryRef+"^{commit}")
	if err != nil || strings.TrimSpace(refHead) != state.CurrentHead {
		return nil, blockAt(req, evidencePath, gitDir, "recovery ref does not fence current destination head", strings.TrimSpace(actualHead), "")
	}
	if state.Completed {
		return state.result(), nil
	}
	if len(state.Items) > 0 && state.Items[len(state.Items)-1].Classification == ReplayUnresolved {
		return nil, blockAt(req, evidencePath, gitDir, "replay state contains unresolved source; explicit recovery required", state.CurrentHead, state.Items[len(state.Items)-1].Source)
	}

	for i := len(state.Items); i < len(sources); i++ {
		source := sources[i]
		head, e := gitText(ctx, req.RepoRoot, "rev-parse", "HEAD")
		if e != nil {
			return nil, e
		}
		if strings.TrimSpace(head) != state.CurrentHead {
			return nil, blockAt(req, evidencePath, gitDir, "destination head changed before ordered replay step", strings.TrimSpace(head), source)
		}
		if empty, e := sourceTreeEqualsParent(ctx, req.RepoRoot, source); e != nil {
			return nil, blockAt(req, evidencePath, gitDir, e.Error(), strings.TrimSpace(head), source)
		} else if empty {
			accounted, e := emptyAnchorParentAccounted(ctx, req.RepoRoot, source, sources[:i])
			if e != nil || !accounted {
				if e == nil {
					e = fmt.Errorf("source %s empty anchor parent is not accounted for by ordered mapping or destination ancestry", source)
				}
				return nil, blockAt(req, evidencePath, gitDir, e.Error(), strings.TrimSpace(head), source)
			}
		}
		item, e := classifyAndApply(ctx, req.RepoRoot, source)
		if e != nil {
			// Persist the causal boundary before emitting BLOCKED evidence. The
			// preceding ordered mappings remain authoritative, while this source
			// is explicitly unresolved and cannot be silently retried or skipped.
			item.Order = i
			item.Classification = ReplayUnresolved
			item.BaseHead = state.CurrentHead
			item.DestinationHead = strings.TrimSpace(head)
			state.Items = append(state.Items, item)
			if saveErr := saveReplayState(req.RepoRoot, statePath, state); saveErr != nil {
				e = errors.Join(e, fmt.Errorf("persist unresolved replay checkpoint: %w", saveErr))
			}
			return nil, blockAt(req, evidencePath, gitDir, e.Error(), strings.TrimSpace(head), source)
		}
		item.Order = i
		item.BaseHead = state.CurrentHead
		newHead, e := gitText(ctx, req.RepoRoot, "rev-parse", "HEAD")
		if e != nil {
			return nil, e
		}
		newHead = strings.TrimSpace(newHead)
		item.DestinationHead = newHead
		if item.Matched == "" {
			item.Matched = newHead
		}
		if err := updateRecoveryRef(ctx, req.RepoRoot, recoveryRef, state.CurrentHead, newHead); err != nil {
			return nil, blockAt(req, evidencePath, gitDir, "post-mutation recovery ref CAS failed: "+err.Error(), newHead, source)
		}
		state.Items = append(state.Items, item)
		state.CurrentHead = newHead
		if e := saveReplayState(req.RepoRoot, statePath, state); e != nil {
			return nil, blockAt(req, evidencePath, gitDir, "post-mutation state persistence failed: "+e.Error(), newHead, source)
		}
	}
	state.Completed = true
	if err := saveReplayState(req.RepoRoot, statePath, state); err != nil {
		return nil, blockAt(req, evidencePath, gitDir, "completion state persistence failed: "+err.Error(), state.CurrentHead, "")
	}
	finalHead, err := gitText(ctx, req.RepoRoot, "rev-parse", "HEAD")
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(finalHead) != state.CurrentHead {
		return nil, blockAt(req, evidencePath, gitDir, "destination head drifted after completion", strings.TrimSpace(finalHead), "")
	}
	return state.result(), nil
}

// RecordReplayBlocked appends a fenced BLOCKED record without changing the
// destination. It is used by downstream gates (for example verification) so
// explicit recovery has durable evidence of why a completed replay cannot be
// published or reconciled.
func RecordReplayBlocked(ctx context.Context, req ReplayRequest, reason string) error {
	if err := validateReplayRequest(req); err != nil {
		return err
	}
	gitDir, err := replayGitDir(ctx, req.RepoRoot)
	if err != nil {
		return err
	}
	commonDir, err := replayCommonDir(ctx, req.RepoRoot)
	if err != nil {
		return err
	}
	raw := digestSources(req.SourceCommits)
	_, evidence, err := replayArtifactPaths(req, raw)
	if err != nil {
		return err
	}
	unlock, err := acquireReplayLock(commonDir)
	if err != nil {
		return blockAt(req, evidence, gitDir, err.Error(), "", "")
	}
	sources, err := resolveSources(ctx, req.RepoRoot, req.SourceCommits)
	if err != nil {
		blocked := blockAt(req, evidence, gitDir, err.Error(), "", "")
		return errors.Join(blocked, unlock())
	}
	digest := digestSources(sources)
	req.sourceDigest = digest
	statePath, evidencePath, err := replayArtifactPaths(req, digest)
	if err != nil {
		return errors.Join(err, unlock())
	}
	state, stateErr := loadReplayState(req.RepoRoot, statePath)
	if stateErr != nil {
		blocked := blockAt(req, evidencePath, gitDir, "state unreadable: "+stateErr.Error(), "", "")
		return errors.Join(blocked, unlock())
	}
	if state == nil {
		blocked := blockAt(req, evidencePath, gitDir, "state missing before downstream block", "", "")
		return errors.Join(blocked, unlock())
	}
	if err := validateReplayState(ctx, req.RepoRoot, state, req, sources, digest); err != nil {
		blocked := blockAt(req, evidencePath, gitDir, err.Error(), "", "")
		return errors.Join(blocked, unlock())
	}
	head, headErr := gitText(ctx, req.RepoRoot, "rev-parse", "HEAD")
	if headErr != nil {
		blocked := blockAt(req, evidencePath, gitDir, "head unreadable before downstream block: "+headErr.Error(), "", "")
		return errors.Join(blocked, unlock())
	}
	ref := replayRecoveryRef(req, digest)
	refHead, refErr := gitText(ctx, req.RepoRoot, "rev-parse", "--verify", ref+"^{commit}")
	if !state.Completed || refErr != nil || strings.TrimSpace(head) != state.CurrentHead || strings.TrimSpace(refHead) != state.CurrentHead {
		blocked := blockAt(req, evidencePath, gitDir, "completed replay fencing proof failed", strings.TrimSpace(head), "")
		return errors.Join(blocked, unlock())
	}
	blocked := blockAt(req, evidencePath, gitDir, reason, strings.TrimSpace(head), "")
	return errors.Join(blocked, unlock())
}

func classifyAndApply(ctx context.Context, repo, source string) (ReplayItem, error) {
	item := ReplayItem{Order: 0, Source: source}
	tree, err := gitText(ctx, repo, "rev-parse", source+"^{tree}")
	if err != nil {
		return item, fmt.Errorf("source %s is not a commit: %w", source, err)
	}
	parent, err := gitText(ctx, repo, "rev-parse", source+"^1^{tree}")
	if err != nil {
		return item, fmt.Errorf("source %s has no parent: %w", source, err)
	}
	if strings.TrimSpace(tree) == strings.TrimSpace(parent) {
		item.Classification = ReplayEmptyAnchor
		return item, nil
	}
	patch, err := patchID(ctx, repo, source)
	if err != nil {
		return item, fmt.Errorf("source %s patch identity: %w", source, err)
	}
	if patch == "" {
		return item, fmt.Errorf("source %s has no nonempty patch identity", source)
	}
	item.PatchID = patch

	exact := gitExit(ctx, repo, "merge-base", "--is-ancestor", source, "HEAD") == nil
	paths, err := commitPaths(ctx, repo, source)
	if err != nil {
		return item, err
	}
	sourceContent, err := contentSignature(ctx, repo, source, paths)
	if err != nil {
		return item, err
	}
	ids, err := destinationPatchMatches(ctx, repo, "HEAD", patch, paths, sourceContent)
	if err != nil {
		return item, err
	}
	if len(ids) > 1 {
		return item, fmt.Errorf("ambiguous duplicate stable patch identity %s", patch)
	}
	if exact {
		if len(ids) != 1 {
			return item, fmt.Errorf("ancestral source %s has non-unique destination mapping", source)
		}
		item.Classification, item.Matched = ReplayAlreadyPresent, ids[0]
		return item, nil
	}
	if len(ids) == 1 {
		item.Classification, item.Matched = ReplayAlreadyPresent, ids[0]
		return item, nil
	}
	if err := replayRunGit(ctx, repo, "cherry-pick", source); err != nil {
		// Do not abort. The sequencer and CHERRY_PICK_HEAD are recovery evidence.
		return item, fmt.Errorf("source %s unresolved: %w", source, err)
	}
	item.Classification = ReplayAppliedExact
	return item, nil
}

func destinationPatchMatches(ctx context.Context, repo, head, want string, paths []string, sourceContent string) ([]string, error) {
	out, err := gitText(ctx, repo, "rev-list", head)
	if err != nil {
		return nil, err
	}
	var matches []string
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		sha := strings.TrimSpace(sc.Text())
		id, e := patchID(ctx, repo, sha)
		if e != nil {
			return nil, e
		}
		if id == "" {
			continue
		}
		if id == want {
			// Stable patch IDs narrow the candidates; touched-path content is
			// the proof. Unrelated destination-only paths are intentionally
			// outside this comparison.
			destinationContent, e := contentSignature(ctx, repo, sha, paths)
			if e != nil {
				return nil, e
			}
			if sourceContent == destinationContent {
				matches = append(matches, sha)
			}
		}
	}
	return matches, sc.Err()
}

func sourceTreeEqualsParent(ctx context.Context, repo, source string) (bool, error) {
	tree, err := gitText(ctx, repo, "rev-parse", source+"^{tree}")
	if err != nil {
		return false, fmt.Errorf("source %s tree: %w", source, err)
	}
	parent, err := gitText(ctx, repo, "rev-parse", source+"^1^{tree}")
	if err != nil {
		return false, fmt.Errorf("source %s parent tree: %w", source, err)
	}
	return tree == parent, nil
}

func emptyAnchorParentAccounted(ctx context.Context, repo, source string, prior []string) (bool, error) {
	return emptyAnchorParentAccountedAt(ctx, repo, source, prior, "HEAD")
}

func emptyAnchorParentAccountedAt(ctx context.Context, repo, source string, prior []string, head string) (bool, error) {
	parent, err := gitText(ctx, repo, "rev-parse", "--verify", "--end-of-options", source+"^1^{commit}")
	if err != nil {
		return false, fmt.Errorf("empty anchor %s parent: %w", source, err)
	}
	for _, earlier := range prior {
		if earlier == parent {
			return true, nil
		}
	}
	if gitExit(ctx, repo, "merge-base", "--is-ancestor", parent, head) == nil {
		return true, nil
	}
	// An independently mapped parent is acceptable when its canonical patch
	// and touched content identify exactly one destination commit.
	patch, err := patchID(ctx, repo, parent)
	if err != nil {
		return false, nil
	}
	paths, err := commitPaths(ctx, repo, parent)
	if err != nil {
		return false, err
	}
	content, err := contentSignature(ctx, repo, parent, paths)
	if err != nil {
		return false, err
	}
	matches, err := destinationPatchMatches(ctx, repo, head, patch, paths, content)
	return len(matches) == 1, err
}

func commitPaths(ctx context.Context, repo, commit string) ([]string, error) {
	out, err := gitBytes(ctx, repo, "diff-tree", "--no-commit-id", "--name-only", "-r", "-z", commit+"^", commit)
	if err != nil {
		return nil, fmt.Errorf("commit %s touched paths: %w", commit, err)
	}
	var paths []string
	for _, line := range bytes.Split(out, []byte{0}) {
		if len(line) != 0 {
			paths = append(paths, string(line))
		}
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("commit %s has no touched paths", commit)
	}
	return paths, nil
}

func contentSignature(ctx context.Context, repo, commit string, paths []string) (string, error) {
	args := append([]string{"ls-tree", "-r", "-z", commit, "--"}, paths...)
	b, err := gitBytes(ctx, repo, args...)
	if err != nil {
		return "", fmt.Errorf("content proof for %s: %w", commit, err)
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func patchID(ctx context.Context, repo, commit string) (string, error) {
	show, err := gitBytes(ctx, repo, "show", "--format=", "--binary", commit)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, "git", "patch-id", "--stable")
	cmd.Dir = repo
	cmd.Stdin = bytes.NewReader(show)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 || strings.Trim(fields[0], "0") == "" {
		return "", nil
	}
	return fields[0], nil
}

func blockAt(req ReplayRequest, evidence, gitDir, reason, actual, source string) error {
	commonDir, commonErr := replayCommonDir(context.Background(), req.RepoRoot)
	if commonErr != nil {
		return fmt.Errorf("%w: evidence common-dir resolution failed", ErrReplayBlocked)
	}
	evidenceUnlock, lockErr := acquireNamedLock(filepath.Join(commonDir, "herd-replay-evidence.lock"))
	if lockErr != nil {
		return lockErr
	}
	seq := []string{}
	for _, p := range []string{"CHERRY_PICK_HEAD", "sequencer/todo", "sequencer/head"} {
		if path, err := replayGitPath(context.Background(), req.RepoRoot, p); err == nil {
			if _, statErr := os.Stat(path); statErr == nil {
				seq = append(seq, "git-path:"+p)
			}
		}
	}
	recovery := req.RecoveryPath
	if recovery == "" {
		recovery = filepath.Join(".herd", strings.Replace(filepath.Base(evidence), "replay-blocked-", "replay-state-", 1))
	}
	digest := req.sourceDigest
	if digest == "" {
		digest = digestSources(req.SourceCommits)
	}
	code, diagnostic := boundedDiagnostic(reason)
	ev := blockedEvidence{Timestamp: time.Now().UTC().Format(time.RFC3339Nano), TaskID: req.TaskID, RepoID: req.RepoID, SourceDigest: digest, Generation: req.Generation, ExpectedHead: req.ExpectedHead, ActualHead: actual, Source: source, Reason: code, DiagnosticDigest: diagnostic, Recovery: recovery + " ref=" + replayRecoveryRef(req, digest), Sequencer: seq}
	data, err := json.Marshal(ev)
	if err != nil {
		return errors.Join(fmt.Errorf("%w: marshal evidence: %v", ErrReplayBlocked, err), evidenceUnlock())
	}
	path := filepath.Join(req.RepoRoot, evidence)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return errors.Join(fmt.Errorf("%w: evidence directory: %v", ErrReplayBlocked, err), evidenceUnlock())
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return errors.Join(fmt.Errorf("%w: open evidence: %v", ErrReplayBlocked, err), evidenceUnlock())
	}
	_, werr := f.Write(append(data, '\n'))
	syncErr := f.Sync()
	_ = f.Close()
	if werr != nil || syncErr != nil {
		return errors.Join(fmt.Errorf("%w: persist evidence: %v", ErrReplayBlocked, errors.Join(werr, syncErr)), evidenceUnlock())
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return errors.Join(fmt.Errorf("%w: evidence directory durability: %v", ErrReplayBlocked, err), evidenceUnlock())
	}
	return errors.Join(fmt.Errorf("%w: %s", ErrReplayBlocked, reason), evidenceUnlock())
}

func loadReplayState(repo, rel string) (*replayState, error) {
	b, err := os.ReadFile(filepath.Join(repo, rel))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var s replayState
	if err := dec.Decode(&s); err != nil {
		return nil, fmt.Errorf("replay state invalid: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return nil, errors.New("replay state has trailing JSON")
	}
	return &s, nil
}

func saveReplayState(repo, rel string, s *replayState) error {
	path := filepath.Join(repo, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".replay-state-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err = f.Chmod(0644); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if _, err = f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err = f.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return syncDir(dir)
}

func (s *replayState) result() *ReplayResult {
	return &ReplayResult{Generation: s.Generation, ExpectedHead: s.ExpectedHead, FinalHead: s.CurrentHead, Items: append([]ReplayItem(nil), s.Items...), Completed: s.Completed}
}

func replayGitDir(ctx context.Context, repo string) (string, error) {
	dir, err := gitText(ctx, repo, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve gitdir: %w", err)
	}
	return filepath.Clean(dir), nil
}

func canonicalRepoIdentity(ctx context.Context, repo string) (string, error) {
	remote, err := gitText(ctx, repo, "remote", "get-url", "origin")
	if err == nil && strings.TrimSpace(remote) != "" {
		if normalized, ok := normalizeRemoteIdentity(strings.TrimSpace(remote)); ok {
			return digestRepoIdentity(normalized), nil
		}
	}
	format, err := gitText(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return "", err
	}
	roots, err := gitText(ctx, repo, "rev-list", "--all", "--max-parents=0")
	if err != nil {
		return "", err
	}
	rootIDs := strings.Fields(roots)
	sort.Strings(rootIDs)
	return digestRepoIdentity("genesis-v1\x00" + format + "\x00" + strings.Join(rootIDs, "\x00")), nil
}

func normalizeRemoteIdentity(raw string) (string, bool) {
	if filepath.IsAbs(raw) || strings.HasPrefix(raw, "file://") {
		return "", false
	}
	if strings.Contains(raw, "://") {
		if u, err := url.Parse(raw); err == nil {
			if u.Host == "" {
				return "", false
			}
			u.User = nil
			u.Host = strings.ToLower(u.Host)
			u.RawQuery, u.Fragment = "", ""
			u.Path = strings.TrimSuffix(u.Path, "/")
			return u.String(), true
		}
	}
	if i := strings.Index(raw, ":"); i > 0 && !strings.Contains(raw[:i], "/") {
		host := raw[:i]
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return "ssh://" + strings.ToLower(host) + "/" + strings.TrimPrefix(raw[i+1:], "/"), true
	}
	return "", false
}

func digestRepoIdentity(canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	return "repo-v1:sha256:" + hex.EncodeToString(h[:])
}

func replayCommonDir(ctx context.Context, repo string) (string, error) {
	dir, err := gitText(ctx, repo, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolve common gitdir: %w", err)
	}
	return filepath.Clean(dir), nil
}

func replayGitPath(ctx context.Context, repo, name string) (string, error) {
	p, err := gitText(ctx, repo, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(repo, p)
	}
	return filepath.Clean(p), nil
}

func acquireReplayLock(gitDir string) (func() error, error) {
	return acquireNamedLock(filepath.Join(gitDir, "herd-replay.lock"))
}

func acquireNamedLock(path string) (func() error, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("open replay lock: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: replay lock is owned by another generation", ErrReplayBlocked)
	}
	return func() error {
		unlockErr := syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		closeErr := f.Close()
		return errors.Join(unlockErr, closeErr)
	}, nil
}

func resolveSources(ctx context.Context, repo string, revisions []string) ([]string, error) {
	resolved := make([]string, 0, len(revisions))
	seen := make(map[string]struct{}, len(revisions))
	for _, revision := range revisions {
		full, err := gitText(ctx, repo, "rev-parse", "--verify", "--end-of-options", revision+"^{commit}")
		if err != nil {
			return nil, fmt.Errorf("resolve source %q: %w", revision, err)
		}
		if full == "" || strings.Trim(strings.ToLower(full), "0123456789abcdef") != "" {
			return nil, fmt.Errorf("source %q did not resolve to a full commit id", revision)
		}
		if _, ok := seen[full]; ok {
			return nil, fmt.Errorf("source revisions resolve to duplicate commit %s", full)
		}
		seen[full] = struct{}{}
		resolved = append(resolved, full)
	}
	return resolved, nil
}

func replayRecoveryRef(req ReplayRequest, digest string) string {
	h := sha256.Sum256([]byte(req.TaskID + "\x00" + req.RepoID + "\x00" + req.Generation + "\x00" + req.ExpectedHead + "\x00" + digest))
	return "refs/herd/replay/" + hex.EncodeToString(h[:])
}

func replayArtifactPaths(req ReplayRequest, digest string) (string, string, error) {
	if req.EvidencePath != "" || req.RecoveryPath != "" {
		return "", "", errors.New("replay artifact paths are coordinator-owned and cannot be caller-selected")
	}
	h := sha256.Sum256([]byte(req.TaskID + "\x00" + req.RepoID + "\x00" + req.Generation + "\x00" + req.ExpectedHead + "\x00" + digest))
	suffix := hex.EncodeToString(h[:])
	return filepath.Join(".herd", "replay-state-"+suffix+".json"), filepath.Join(".herd", "replay-blocked-"+suffix+".jsonl"), nil
}

func updateRecoveryRef(ctx context.Context, repo, ref, old, next string) error {
	args := []string{"update-ref", "--no-deref", ref, next}
	args = append(args, old)
	return replayRunGit(ctx, repo, args...)
}

func zeroObjectID(ctx context.Context, repo string) (string, error) {
	format, err := gitText(ctx, repo, "rev-parse", "--show-object-format")
	if err != nil {
		return "", fmt.Errorf("resolve object format: %w", err)
	}
	switch strings.TrimSpace(format) {
	case "sha1":
		return strings.Repeat("0", 40), nil
	case "sha256":
		return strings.Repeat("0", 64), nil
	default:
		return "", fmt.Errorf("unsupported git object format %q", format)
	}
}

func validateExpectedHead(ctx context.Context, repo, expected string) error {
	resolved, err := gitText(ctx, repo, "rev-parse", "--verify", "--end-of-options", expected+"^{commit}")
	if err != nil {
		return fmt.Errorf("expected destination head is not a commit: %w", err)
	}
	if resolved != expected {
		return fmt.Errorf("expected destination head must be the full object id: got %s", expected)
	}
	return nil
}

func validateReplayState(ctx context.Context, repo string, s *replayState, req ReplayRequest, sources []string, digest string) error {
	if s.Version != replayStateVersion {
		return fmt.Errorf("unsupported replay state version %d", s.Version)
	}
	if s.Generation != req.Generation || s.TaskID != req.TaskID || s.RepoID != req.RepoID || s.ExpectedHead != req.ExpectedHead || s.SourceDigest != digest {
		return errors.New("replay generation, expected head, or pinned source list changed")
	}
	if len(s.Sources) != len(sources) {
		return errors.New("replay state source count mismatch")
	}
	for i := range sources {
		if s.Sources[i] != sources[i] {
			return fmt.Errorf("replay state source order mismatch at %d", i)
		}
	}
	if len(s.Items) > len(s.Sources) || (s.Completed && len(s.Items) != len(s.Sources)) {
		return errors.New("replay state has too many applied items")
	}
	previous := s.ExpectedHead
	for i, item := range s.Items {
		if item.Order != i || item.Source != sources[i] || item.BaseHead != previous || item.BaseHead == "" || item.DestinationHead == "" || item.Classification == "" {
			return fmt.Errorf("replay state item %d invariant failed", i)
		}
		if _, err := gitText(ctx, repo, "rev-parse", "--verify", item.DestinationHead+"^{commit}"); err != nil {
			return fmt.Errorf("replay state destination %d is missing", i)
		}
		if item.Classification == ReplayEmptyAnchor || item.Classification == ReplayAlreadyPresent {
			if item.DestinationHead != item.BaseHead || item.Matched == "" {
				return fmt.Errorf("replay state unchanged mapping %d invariant failed", i)
			}
		} else if item.Classification == ReplayAppliedExact {
			if item.Matched != item.DestinationHead || item.PatchID == "" {
				return fmt.Errorf("replay state applied mapping %d invariant failed", i)
			}
		} else if item.Classification == ReplayUnresolved {
			if i != len(s.Items)-1 || item.DestinationHead != item.BaseHead || item.Matched != "" {
				return fmt.Errorf("replay state unresolved mapping %d invariant failed", i)
			}
		} else {
			return fmt.Errorf("replay state classification %q is not resumable", item.Classification)
		}
		if item.Classification == ReplayEmptyAnchor {
			ok, err := emptyAnchorParentAccountedAt(ctx, repo, item.Source, sources[:i], item.BaseHead)
			if err != nil || !ok {
				return fmt.Errorf("replay state empty-anchor mapping %d parent proof failed", i)
			}
		}
		if item.Classification != ReplayEmptyAnchor {
			patch, err := patchID(ctx, repo, item.Source)
			if err != nil || patch != item.PatchID {
				return fmt.Errorf("replay state source mapping %d patch proof failed", i)
			}
			paths, err := commitPaths(ctx, repo, item.Source)
			if err != nil {
				return err
			}
			content, err := contentSignature(ctx, repo, item.Source, paths)
			if err != nil {
				return err
			}
			matches, err := destinationPatchMatches(ctx, repo, item.DestinationHead, item.PatchID, paths, content)
			if err != nil || len(matches) != 1 || matches[0] != item.Matched {
				return fmt.Errorf("replay state source mapping %d content proof failed", i)
			}
			if item.Classification == ReplayAppliedExact {
				parent, err := gitText(ctx, repo, "rev-parse", "--verify", "--end-of-options", item.DestinationHead+"^1^{commit}")
				if err != nil || parent != item.BaseHead {
					return fmt.Errorf("replay state applied mapping %d parent chain failed", i)
				}
			}
		}
		previous = item.DestinationHead
	}
	if s.CurrentHead == "" || s.CurrentHead != previous || (len(s.Items) == 0 && s.CurrentHead != s.ExpectedHead) {
		return errors.New("replay state current head is empty")
	}
	return nil
}

func secureRepoPath(repo, rel string) error {
	root, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	target := filepath.Join(root, filepath.Clean(rel))
	if outside, _ := filepath.Rel(root, target); outside == ".." || strings.HasPrefix(outside, ".."+string(filepath.Separator)) {
		return fmt.Errorf("replay path escapes repository: %s", rel)
	}
	for p := target; ; p = filepath.Dir(p) {
		info, statErr := os.Lstat(p)
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("replay path contains symlink: %s", rel)
		}
		if p == root || p == filepath.Dir(p) {
			break
		}
	}
	return nil
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func boundedDiagnostic(reason string) (string, string) {
	full := reason
	bounded := full
	if len(bounded) > 4096 {
		bounded = bounded[:4096]
	}
	lower := strings.ToLower(bounded)
	code := "replay_blocked"
	if strings.Contains(lower, "verifier failed") {
		code = "replay_verifier_failed"
	} else if strings.Contains(lower, "verifier error") {
		code = "replay_verifier_error"
	}
	for _, candidate := range []string{"unresolved", "verifier", "stale", "ambiguous", "empty_anchor", "recovery", "lock", "state", "head"} {
		if strings.HasPrefix(code, "replay_verifier_") {
			break
		}
		if strings.Contains(lower, candidate) {
			code = "replay_" + candidate
			break
		}
	}
	h := sha256.Sum256([]byte(full))
	return code, hex.EncodeToString(h[:])
}

func validateReplayRequest(r ReplayRequest) error {
	if strings.TrimSpace(r.RepoRoot) == "" || strings.TrimSpace(r.TaskID) == "" || strings.TrimSpace(r.RepoID) == "" || strings.TrimSpace(r.ExpectedHead) == "" || strings.TrimSpace(r.Generation) == "" {
		return errors.New("replay requires repo root, task identity, repository identity, expected head, and generation")
	}
	if filepath.IsAbs(r.TaskID) || filepath.IsAbs(r.RepoID) {
		return errors.New("replay identities must be portable, not absolute paths")
	}
	if err := validateOpaqueIdentity("task id", r.TaskID, 128, false); err != nil {
		return err
	}
	if err := validateOpaqueIdentity("generation", r.Generation, 256, true); err != nil {
		return err
	}
	if !strings.HasPrefix(r.RepoID, "repo-v1:sha256:") || len(r.RepoID) != len("repo-v1:sha256:")+64 || strings.Trim(strings.TrimPrefix(r.RepoID, "repo-v1:sha256:"), "0123456789abcdef") != "" {
		return errors.New("repository identity must be repo-v1:sha256:<64 lowercase hex>")
	}
	if len(r.SourceCommits) == 0 {
		return errors.New("replay requires an ordered source commit list")
	}
	seen := map[string]bool{}
	for _, s := range r.SourceCommits {
		if strings.TrimSpace(s) == "" || seen[s] {
			return fmt.Errorf("duplicate or empty source commit %q", s)
		}
		seen[s] = true
	}
	return nil
}

func validateOpaqueIdentity(label, value string, max int, allowSlash bool) error {
	if value == "" || len(value) > max {
		return fmt.Errorf("%s is empty or oversized", label)
	}
	for _, r := range value {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("%s contains control characters", label)
		}
	}
	if strings.Contains(value, "..") || (!allowSlash && (strings.ContainsAny(value, "/\\") || filepath.IsAbs(value))) {
		return fmt.Errorf("%s contains traversal-like material", label)
	}
	return nil
}

func relativePath(p string) error {
	if filepath.IsAbs(p) || p == "." || strings.HasPrefix(filepath.Clean(p), ".."+string(filepath.Separator)) {
		return fmt.Errorf("replay paths must be repo-relative: %s", p)
	}
	return nil
}
func digestSources(s []string) string {
	h := sha256.New()
	for _, v := range s {
		_, _ = io.WriteString(h, v+"\x00")
	}
	return hex.EncodeToString(h.Sum(nil))
}
func gitText(ctx context.Context, repo string, args ...string) (string, error) {
	b, err := gitBytes(ctx, repo, args...)
	return strings.TrimSpace(string(b)), err
}
func gitBytes(ctx context.Context, repo string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = repo
	b, err := c.Output()
	if err != nil {
		return nil, fmt.Errorf("git %v: %w", args, err)
	}
	return b, nil
}
func replayRunGit(ctx context.Context, repo string, args ...string) error {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = repo
	b, err := c.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git %v: %w: %s", args, err, strings.TrimSpace(string(b)))
	}
	return nil
}

func gitExit(ctx context.Context, repo string, args ...string) error {
	c := exec.CommandContext(ctx, "git", args...)
	c.Dir = repo
	return c.Run()
}
