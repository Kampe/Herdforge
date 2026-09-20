package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/refname"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
)

const taskSourceJournal = ".herd/source/task-worktrees.jsonl"
const taskSourceMarker = "herd-task-source.json"
const taskSourceLimit = 1 << 20

// This is a coordinator-signed adoption of one EXISTING registration, not a
// launch receipt or deletion authority. A private registration marker makes
// remove/recreate at the same path lose the adoption, even at the same SHA.
type taskSourceBinding struct {
	Version       int    `json:"version"`
	Repository    string `json:"repository"`
	Worktree      string `json:"worktree"`
	Registration  string `json:"registration"`
	Branch        string `json:"branch"`
	Head          string `json:"head"`
	Task          string `json:"task"`
	Receipt       string `json:"receipt"`
	ReceiptDigest string `json:"receipt_digest"`
	LaunchDigest  string `json:"launch_digest"`
	Generation    string `json:"generation"`
	Signature     string `json:"signature"`
}

func (b taskSourceBinding) signedBytes() []byte {
	b.Signature = ""
	raw, _ := json.Marshal(b)
	return append([]byte("herd/completed-task-worktree/v1\n"), raw...)
}

type taskSourceView struct {
	root       string
	repository string
	bindings   map[string]taskSourceBinding
	homes      []string
	err        error
}

// The live census is a single read per adoption/report/beat, not one Herdr
// subprocess per registration. An unreadable census never permits an override.
var taskSourceAgents = func() ([]herdr.AgentEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	agents, identified, err := herdr.AgentListVerifiedContext(ctx)
	if err != nil {
		return nil, err
	}
	if !identified {
		return nil, fmt.Errorf("task source: live roster identity is unverified")
	}
	return agents, nil
}

func reapInvokingHome(path string) bool {
	cwd, err := os.Getwd()
	if err != nil {
		return true
	}
	cwd, err = canonicalWorktreePath(cwd)
	if err != nil {
		return true
	}
	target, err := canonicalWorktreePath(path)
	return err != nil || cwd == target || strings.HasPrefix(cwd, target+string(filepath.Separator))
}

// Candidacy is an in-memory name lookup, never generation or deletion proof.
func (v *taskSourceView) names(e worktreeEntry) bool {
	if v == nil || v.err != nil {
		return false
	}
	rel, err := filepath.Rel(v.root, e.Path)
	if err != nil {
		return false
	}
	b, ok := v.bindings[rel]
	return ok && b.Branch == e.Branch && b.Head == e.Head
}

func taskSourceRoot(root string) (string, error) {
	common, err := gitroot.CommonDir(context.Background(), root)
	if err != nil {
		return "", err
	}
	return canonicalWorktreePath(filepath.Dir(common))
}

func taskSourceRead(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, fmt.Errorf("task source: unsafe or oversized file %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, fmt.Errorf("task source: file changed while opening %s", path)
	}
	raw, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("task source: file exceeds byte limit")
	}
	return raw, nil
}

func loadTaskSources(root string) *taskSourceView {
	v := &taskSourceView{bindings: map[string]taskSourceBinding{}}
	// No journal means no new authority and no fleet/config/key work. This is
	// also the legacy behavior for installations that never adopted a source.
	canonical, err := taskSourceRoot(root)
	if err != nil {
		v.err = err
		return v
	}
	v.root = canonical
	if err := taskSourceUnaliased(filepath.Dir(filepath.Join(canonical, taskSourceJournal))); err != nil {
		v.err = err
		return v
	}
	raw, err := taskSourceRead(filepath.Join(canonical, taskSourceJournal), taskSourceLimit)
	if os.IsNotExist(err) {
		return v
	}
	if err != nil {
		v.err = err
		return v
	}
	v.repository, v.err = dispatch.AuthenticatedRepositoryIdentity(canonical)
	if v.err != nil {
		return v
	}
	verifier, err := dispatch.LoadVerifier(canonical)
	if err != nil {
		v.err = err
		return v
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	for count := 0; scanner.Scan(); count++ {
		var b taskSourceBinding
		if count >= 256 {
			v.err = fmt.Errorf("task source: journal exceeds 256 records")
			return v
		}
		if err := json.Unmarshal(scanner.Bytes(), &b); err != nil {
			v.err = err
			return v
		}
		if b.Version != 1 || b.Repository != v.repository || b.Generation == "" || b.Task == "" || b.ReceiptDigest == "" || b.LaunchDigest == "" || b.Registration == "" || len(b.Head) != 40 {
			v.err = fmt.Errorf("task source: incomplete or foreign adoption")
			return v
		}
		if err := verifier.VerifyBytes(b.signedBytes(), b.Signature); err != nil {
			v.err = err
			return v
		}
		if _, err := taskSourceManagedPath(canonical, b.Worktree); err != nil {
			v.err = err
			return v
		}
		v.bindings[b.Worktree] = b
	}
	if v.err = scanner.Err(); v.err != nil {
		return v
	}
	v.homes, v.err = taskSourceHomes(canonical)
	return v
}

func taskSourceManagedPath(root, relative string) (string, error) {
	clean := filepath.Clean(relative)
	if filepath.IsAbs(relative) || clean != relative || (!strings.HasPrefix(clean, ".worktrees/") && !strings.HasPrefix(clean, ".herd/worktrees/")) {
		return "", fmt.Errorf("task source: target must be an exact managed repository-relative path")
	}
	path, err := canonicalWorktreePath(filepath.Join(root, relative))
	if err != nil {
		return "", err
	}
	if path != filepath.Join(root, relative) {
		return "", fmt.Errorf("task source: symlinked or escaping managed path")
	}
	return path, nil
}

func taskSourceHomes(root string) ([]string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	homes := []string{root, cwd}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		return nil, fmt.Errorf("task source: resident configuration unknown: %w", err)
	}
	if cfg == nil {
		return nil, fmt.Errorf("task source: resident configuration unavailable")
	}
	for _, lane := range cfg.Lanes {
		if lane.Worktree == "" {
			continue
		}
		path := lane.Worktree
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		homes = append(homes, path)
	}
	agents, err := taskSourceAgents()
	if err != nil {
		return nil, fmt.Errorf("task source: live homes unknown: %w", err)
	}
	for _, agent := range agents {
		if agent.Cwd == "" {
			return nil, fmt.Errorf("task source: live home has no cwd")
		}
		homes = append(homes, agent.Cwd)
		if agent.ForegroundCwd != "" {
			homes = append(homes, agent.ForegroundCwd)
		}
	}
	for i, home := range homes {
		resolved, err := canonicalWorktreePath(home)
		if err != nil {
			return nil, fmt.Errorf("task source: resident identity unknown: %w", err)
		}
		homes[i] = resolved
	}
	return homes, nil
}

func (v *taskSourceView) hardHome(e worktreeEntry) error {
	if v == nil || v.err != nil {
		return fmt.Errorf("task source: resident authority unavailable")
	}
	b := strings.ToLower(strings.TrimSpace(e.Branch))
	if e.IsMain || e.Detached || b == "main" || b == "master" || refname.IsStandingBranch(b) {
		return fmt.Errorf("task source: canonical, detached or standing checkout")
	}
	path, err := canonicalWorktreePath(e.Path)
	if err != nil {
		return err
	}
	for _, home := range v.homes {
		if home == path || strings.HasPrefix(home, path+string(filepath.Separator)) {
			return fmt.Errorf("task source: invoking, configured or live resident home")
		}
	}
	// These independently named resident homes remain protected even if a
	// valid completed candidate is checked out there and no agent is running.
	switch strings.ToLower(filepath.Base(path)) {
	case "orchestrator", "coordinator", "supervisor", "herd-smith":
		return fmt.Errorf("task source: reserved resident home")
	}
	return nil
}

func (v *taskSourceView) authorize(e worktreeEntry) (*taskSourceBinding, error) {
	if err := v.hardHome(e); err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(v.root, e.Path)
	if err != nil {
		return nil, err
	}
	b, found := v.bindings[rel]
	if !found {
		return nil, fmt.Errorf("task source: no signed adoption")
	}
	path, err := taskSourceManagedPath(v.root, b.Worktree)
	if err != nil {
		return nil, err
	}
	if path != e.Path || b.Branch != e.Branch || b.Head != e.Head {
		return nil, fmt.Errorf("task source: path, branch or head changed")
	}
	admin, err := herdr.HarvestRegistrationDir(path)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(admin) != filepath.Join(v.root, ".git", "worktrees") {
		return nil, fmt.Errorf("task source: foreign registration")
	}
	if filepath.Base(admin) != b.Registration {
		return nil, fmt.Errorf("task source: registration changed")
	}
	raw, err := taskSourceRead(filepath.Join(admin, taskSourceMarker), 16384)
	if err != nil {
		return nil, fmt.Errorf("task source: generation marker unavailable: %w", err)
	}
	var marker taskSourceBinding
	if json.Unmarshal(raw, &marker) != nil || marker != b {
		return nil, fmt.Errorf("task source: generation marker changed")
	}
	receipt, err := readTaskSourceReceipt(v.root, b.Receipt)
	if err != nil || receipt.Digest != b.ReceiptDigest || receipt.TaskRef != b.Task || receipt.CandidateSHA != b.Head {
		return nil, fmt.Errorf("task source: completion binding changed")
	}
	return &b, nil
}

func readTaskSourceReceipt(root, relative string) (*hsync.CompletionReceipt, error) {
	if relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || strings.HasPrefix(relative, "../") {
		return nil, fmt.Errorf("task source: receipt must be repository-relative")
	}
	path, err := canonicalWorktreePath(filepath.Join(root, relative))
	if err != nil || path != filepath.Join(root, relative) {
		return nil, fmt.Errorf("task source: receipt path changed or escaped")
	}
	raw, err := taskSourceRead(path, 65536)
	if err != nil {
		return nil, err
	}
	var receipt hsync.CompletionReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return nil, err
	}
	if receipt.Digest == "" || receipt.Digest != receipt.ComputeDigest() {
		return nil, fmt.Errorf("task source: completion digest invalid")
	}
	return &receipt, nil
}

// Completion authentication and the canonical independent-review read are
// deliberately reused. A sealed JSON digest by itself is never adoption proof.
func validateTaskSourceCompletion(root, ref, relative string) (*hsync.CompletionReceipt, error) {
	receipt, err := readTaskSourceReceipt(root, relative)
	if err != nil {
		return nil, err
	}
	req, close, err := buildDoneRequest(root, "", ref, filepath.Join(root, relative), "", nil)
	if err != nil {
		return nil, err
	}
	defer close()
	if req.Receipt == nil || req.Receipt.Digest != receipt.Digest {
		return nil, fmt.Errorf("task source: completion changed during validation")
	}
	if err := hsync.AuthenticateDoneReceipt(req, root, ref); err != nil {
		return nil, err
	}
	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, reviewledger.PathFor(root))
	if err != nil {
		return nil, err
	}
	result, err := ledger.AdmitReduced(reviewledger.ReducedAdmissionOpts{CandidateSHA: receipt.CandidateSHA, ReconcileConsumedMergeSHA: receipt.MergeSHA})
	if err != nil {
		return nil, fmt.Errorf("task source: canonical review refused: %w", err)
	}
	if result == nil || !result.Admitted || result.VerificationDigest != receipt.VerificationDigest || result.AuthorFamily != receipt.AuthorFamily || result.ReviewerFamily != receipt.ReviewerFamily || result.Tier != receipt.RiskTier {
		return nil, fmt.Errorf("task source: completion differs from independent review")
	}
	verdict, found, err := ledger.VerdictForReviewer(receipt.CandidateSHA, result.Reviewer)
	if err != nil || !found || reviewledger.CloseableCardRef(verdict.Task) != reviewledger.CloseableCardRef(ref) || reviewledger.CloseableCardRef(ref) == "" {
		return nil, fmt.Errorf("task source: independent review does not bind this task")
	}
	return receipt, nil
}

func taskSourceLaunchDigest(root string, e worktreeEntry) (string, error) {
	repository, err := dispatch.AuthenticatedRepositoryIdentity(root)
	if err != nil {
		return "", err
	}
	raw, err := taskSourceRead(launch.ReceiptPathFor(root), 8<<20)
	if err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 65536)
	var matched string
	for scanner.Scan() {
		var r launch.Receipt
		if err := json.Unmarshal(scanner.Bytes(), &r); err != nil {
			return "", err
		}
		if !r.Accepted || r.Branch != e.Branch || r.Name == "" {
			continue
		}
		path := r.Worktree
		if path == "" {
			path = r.CWD
		}
		if path == "" {
			continue
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		resolved, err := canonicalWorktreePath(path)
		if err != nil {
			return "", err
		}
		if resolved != e.Path {
			continue
		}
		if r.Repository != "" && r.Repository != repository {
			return "", fmt.Errorf("task source: foreign launch provenance")
		}
		sum := sha256.Sum256(scanner.Bytes())
		matched = hex.EncodeToString(sum[:])
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	if matched == "" {
		return "", fmt.Errorf("task source: no accepted original path/branch launch provenance")
	}
	return matched, nil
}

func enrollTaskSource(root, target, ref, receiptPath string, write bool) (*taskSourceBinding, error) {
	canonical, err := taskSourceRoot(root)
	if err != nil {
		return nil, err
	}
	path, err := taskSourceManagedPath(canonical, target)
	if err != nil {
		return nil, err
	}
	registrations, err := reapRegistrationLister(canonical)
	if err != nil {
		return nil, err
	}
	e, found := exactWorktreeEntry(registrations, path)
	if !found {
		return nil, fmt.Errorf("task source: exact registration is missing")
	}
	e = inspectWorktreeEntries([]worktreeEntry{e})[0]
	if e.StatusError != "" || e.Dirty || e.Locked {
		return nil, fmt.Errorf("task source: source is dirty, locked or unknown")
	}
	admin, err := herdr.HarvestRegistrationDir(path)
	if err != nil {
		return nil, err
	}
	if filepath.Dir(admin) != filepath.Join(canonical, ".git", "worktrees") {
		return nil, fmt.Errorf("task source: foreign registration")
	}
	registration, err := os.Stat(admin)
	if err != nil {
		return nil, err
	}
	v := loadTaskSources(canonical)
	if v.err != nil {
		return nil, v.err
	}
	if len(v.homes) == 0 {
		v.homes, v.err = taskSourceHomes(canonical)
	}
	if err := v.hardHome(e); err != nil {
		return nil, err
	}
	receipt, err := validateTaskSourceCompletion(canonical, ref, receiptPath)
	if err != nil {
		return nil, err
	}
	if receipt.CandidateSHA != e.Head {
		return nil, fmt.Errorf("task source: completion candidate differs from registered head")
	}
	if commitsAhead(canonical, "origin/main", e.Branch) != 0 {
		if _, err := rangeLandedProof(canonical, "origin/main", e.Branch); err != nil {
			return nil, fmt.Errorf("task source: current landing refused: %w", err)
		}
	}
	if err := reapHarvestDurableHold(canonical, path); err != nil {
		return nil, err
	}
	provenance, err := taskSourceLaunchDigest(canonical, e)
	if err != nil {
		return nil, err
	}
	repository, err := dispatch.AuthenticatedRepositoryIdentity(canonical)
	if err != nil {
		return nil, err
	}
	b := taskSourceBinding{Version: 1, Repository: repository, Worktree: target, Registration: filepath.Base(admin), Branch: e.Branch, Head: e.Head, Task: receipt.TaskRef, Receipt: receiptPath, ReceiptDigest: receipt.Digest, LaunchDigest: provenance}
	if !write {
		return &b, nil
	}
	return publishTaskSource(canonical, admin, registration, b)
}

func publishTaskSource(root, admin string, registration os.FileInfo, b taskSourceBinding) (*taskSourceBinding, error) {
	signer, err := dispatch.LoadSignerForConfig("", root)
	if err != nil {
		return nil, err
	}
	journal := filepath.Join(root, taskSourceJournal)
	if err := taskSourceUnaliased(filepath.Dir(journal)); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(journal), 0700); err != nil {
		return nil, err
	}
	if err := taskSourceUnaliased(filepath.Dir(journal)); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(journal+".lock", os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return nil, fmt.Errorf("task source: adoption busy: %w", err)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Revalidate after acquiring the publication lock. A removed/recreated
	// registration never inherits the earlier observation, even at the same SHA.
	entries, err := reapRegistrationLister(root)
	if err != nil {
		return nil, err
	}
	e, found := exactWorktreeEntry(entries, filepath.Join(root, b.Worktree))
	if !found || e.Branch != b.Branch || e.Head != b.Head {
		return nil, fmt.Errorf("task source: registration changed before publication")
	}
	e = inspectWorktreeEntries([]worktreeEntry{e})[0]
	if e.Locked || e.Dirty || e.StatusError != "" {
		return nil, fmt.Errorf("task source: carrier changed before publication")
	}
	v := loadTaskSources(root)
	if v.err != nil {
		return nil, v.err
	}
	if len(v.homes) == 0 {
		v.homes, v.err = taskSourceHomes(root)
	}
	if err := v.hardHome(e); err != nil {
		return nil, err
	}
	currentAdmin, err := herdr.HarvestRegistrationDir(e.Path)
	if err != nil || currentAdmin != admin {
		return nil, fmt.Errorf("task source: registration path changed")
	}
	current, err := os.Stat(admin)
	if err != nil || !os.SameFile(registration, current) {
		return nil, fmt.Errorf("task source: registration generation changed")
	}
	completion, err := validateTaskSourceCompletion(root, b.Task, b.Receipt)
	if err != nil {
		return nil, err
	}
	if completion.Digest != b.ReceiptDigest {
		return nil, fmt.Errorf("task source: completion changed before publication")
	}
	if err := reapHarvestDurableHold(root, e.Path); err != nil {
		return nil, err
	}
	provenance, err := taskSourceLaunchDigest(root, e)
	if err != nil || provenance != b.LaunchDigest {
		return nil, fmt.Errorf("task source: launch provenance changed before publication")
	}
	current, err = os.Stat(admin)
	if err != nil || !os.SameFile(registration, current) {
		return nil, fmt.Errorf("task source: registration generation changed before marker write")
	}
	markerPath := filepath.Join(admin, taskSourceMarker)
	if raw, err := taskSourceRead(markerPath, 16384); err == nil {
		var prior taskSourceBinding
		if json.Unmarshal(raw, &prior) != nil {
			return nil, fmt.Errorf("task source: invalid existing marker")
		}
		b.Generation, b.Signature = prior.Generation, prior.Signature
		if prior != b {
			return nil, fmt.Errorf("task source: existing generation has different evidence")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else {
		nonce := make([]byte, 32)
		if _, err := rand.Read(nonce); err != nil {
			return nil, err
		}
		b.Generation = hex.EncodeToString(nonce)
		b.Signature, err = signer.SignBytes(b.signedBytes())
		if err != nil {
			return nil, err
		}
		raw, _ := json.Marshal(b)
		if err := writeTaskSourceExclusive(markerPath, raw); err != nil {
			return nil, err
		}
	}
	verifier, err := dispatch.LoadVerifier(root)
	if err != nil {
		return nil, err
	}
	if err := verifier.VerifyBytes(b.signedBytes(), b.Signature); err != nil {
		return nil, err
	}
	raw, err := taskSourceRead(journal, taskSourceLimit)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	encoded, _ := json.Marshal(b)
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if bytes.Equal(line, encoded) {
			return &b, nil
		}
	}
	if len(raw)+len(encoded)+1 > taskSourceLimit || bytes.Count(raw, []byte{'\n'}) >= 256 {
		return nil, fmt.Errorf("task source: enrollment journal is full; marker retained without new published authority")
	}
	f, err := os.OpenFile(journal, os.O_CREATE|os.O_APPEND|os.O_WRONLY|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	_, writeErr := f.Write(append(encoded, '\n'))
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return nil, fmt.Errorf("task source: marker retained; journal publication uncertain: %w", err)
	}
	if err := syncTaskSourceDir(filepath.Dir(journal)); err != nil {
		return nil, fmt.Errorf("task source: journal written; durability uncertain: %w", err)
	}
	// Read-back reports publication only when both halves still agree. A
	// failure here may have published authority and is never called success.
	readback := loadTaskSources(root)
	current, err = os.Stat(admin)
	if err != nil || !os.SameFile(registration, current) {
		return nil, fmt.Errorf("task source: publication uncertain; registration generation changed")
	}
	actual, err := readback.authorize(e)
	if err != nil {
		return nil, fmt.Errorf("task source: journal written; read-back refused: %w", err)
	}
	if *actual != b {
		return nil, fmt.Errorf("task source: journal written; read-back identity differs")
	}
	return &b, nil
}

func taskSourceUnaliased(path string) error {
	for cursor := filepath.Clean(path); ; cursor = filepath.Dir(cursor) {
		info, err := os.Lstat(cursor)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
			return fmt.Errorf("task source: aliased or non-directory authority path")
		}
		if filepath.Dir(cursor) == cursor {
			return nil
		}
	}
}

func writeTaskSourceExclusive(path string, raw []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	if writeErr == nil {
		writeErr = f.Sync()
	}
	closeErr := f.Close()
	if err := errors.Join(writeErr, closeErr); err != nil {
		return err
	}
	return syncTaskSourceDir(filepath.Dir(path))
}

func syncTaskSourceDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// Revalidate the signed distinction at the destructive fence. Existing reap
// checks still prove cleanliness, owners, current landing, and exact Git CAS.
func validateTaskSourceAct(v *taskSourceView, e worktreeEntry, expected taskSourceBinding) error {
	actual, err := v.authorize(e)
	if err != nil {
		return err
	}
	if *actual != expected {
		return fmt.Errorf("task source: adoption changed after classification")
	}
	if _, err := validateTaskSourceCompletion(v.root, actual.Task, actual.Receipt); err != nil {
		return err
	}
	provenance, err := taskSourceLaunchDigest(v.root, e)
	if err != nil || provenance != actual.LaunchDigest {
		return fmt.Errorf("task source: launch provenance changed")
	}
	return nil
}
