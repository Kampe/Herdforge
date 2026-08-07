package mutationprobe

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// KnownProbePrefixes are leaf-name prefixes this package will create and
// the legacy FAC-83 shape it will only report (never auto-delete).
var KnownProbePrefixes = []string{
	"herd-mutprobe.",
	"fac83-probe.",
}

// CreateRequest identifies one mutation probe to materialize.
type CreateRequest struct {
	TaskRef      string
	Generation   string
	CandidateSHA string
	// ProbeID is optional; when empty a random id is generated.
	ProbeID string
}

// Probe is the live handle returned after a successful Create. Path is
// absolute for the current process only and is never persisted.
type Probe struct {
	ID           string
	TaskRef      string
	Generation   string
	CandidateSHA string
	ProbeName    string
	Path         string // absolute runtime path under Manager.TempRoot
}

// Classification is the fail-closed evidence used before cleanup.
type Classification struct {
	Class   Class
	HEAD    string
	Dirty   bool
	Reason  string
	// PreserveAction is operator-facing recovery guidance; free of host paths.
	PreserveAction string
}

// RecoveryReport is a read-only description of a leaked or preserved probe
// (including the FAC-83 shape). It never deletes anything.
type RecoveryReport struct {
	ProbeName     string `json:"probe_name"`
	TaskRef       string `json:"task_ref,omitempty"`
	CandidateSHA  string `json:"candidate_sha,omitempty"`
	Generation    string `json:"generation,omitempty"`
	Class         Class  `json:"class"`
	HEAD          string `json:"head,omitempty"`
	Dirty         bool   `json:"dirty"`
	Registered    bool   `json:"registered_in_origin"`
	DirectoryExists bool `json:"directory_exists"`
	Reason        string `json:"reason"`
	PreserveAction string `json:"preserve_action"`
	// PathLeaf is the portable basename only — never a host absolute path.
	PathLeaf string `json:"path_leaf"`
}

// Manager owns create/classify/remove against one hermetic origin repository
// and one temp root. Production callers pass the repository root and leave
// TempRoot empty (defaults to os.TempDir()). Tests inject a TempRoot under
// t.TempDir so git worktree mutations never touch the developer checkout.
//
// Git worktree add/remove against a shared origin is serialized via mu:
// concurrent mutations corrupt .git/worktrees metadata (observed under
// race/stress as "failed to read .../commondir").
type Manager struct {
	OriginRoot    string
	TempRoot      string
	DiskAdmission resources.DiskAdmission
	// execCommandContext is injectable so tests can fail closed before git.
	execCommandContext func(context.Context, string, ...string) *exec.Cmd
	// BeforeRemoveFunc is a final-boundary seam used by non-vacuity tests to
	// inject a late dirty write after classify and before remove.
	BeforeRemoveFunc func(context.Context, string) error
	// SkipCleanup is a test-only seam: when true, EnsureCleanup classifies
	// but never removes, proving the suite fails if the reap barrier is
	// removed.
	SkipCleanup bool

	mu sync.Mutex
}

// NewManager returns a Manager bound to originRoot. tempRoot may be empty.
func NewManager(originRoot, tempRoot string) *Manager {
	m := &Manager{
		OriginRoot:         originRoot,
		TempRoot:           tempRoot,
		execCommandContext: exec.CommandContext,
	}
	m.configureDefaultDiskAdmission()
	return m
}

func (m *Manager) configureDefaultDiskAdmission() {
	m.DiskAdmission = resources.NewCapacityGate(resources.OSBackend{}, resources.DefaultDiskPolicy())
}

func (m *Manager) commandContext() func(context.Context, string, ...string) *exec.Cmd {
	if m != nil && m.execCommandContext != nil {
		return m.execCommandContext
	}
	return exec.CommandContext
}

func (m *Manager) tempRoot() (string, error) {
	if m == nil {
		return "", fmt.Errorf("mutationprobe: nil manager")
	}
	root := m.TempRoot
	if root == "" {
		root = os.TempDir()
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("mutationprobe: resolve temp root: %w", err)
	}
	return abs, nil
}

func (m *Manager) admitDisk(targetPath string) error {
	if m == nil {
		return fmt.Errorf("mutationprobe: nil manager")
	}
	if m.DiskAdmission == nil {
		m.configureDefaultDiskAdmission()
	}
	origin, err := resources.ResolveExistingPath(m.OriginRoot)
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve origin volume: %w", err)
	}
	tmpParent := filepath.Dir(targetPath)
	tmp, err := resources.ResolveExistingPath(tmpParent)
	if err != nil {
		return fmt.Errorf("disk capacity gate: resolve temp volume: %w", err)
	}
	requirement, err := resources.AggregateDiskRequirement(resources.DefaultWorktreeCreateRequirement())
	if err != nil {
		return fmt.Errorf("disk capacity gate: invalid requirement")
	}
	decision := m.DiskAdmission.Admit(resources.DiskRequest{
		Operation:     "mutation_probe_create",
		Path:          origin,
		TempPath:      tmp,
		RequiredBytes: requirement.Bytes,
		RequiredInodes: requirement.Inodes,
	})
	if decision.Allowed {
		return nil
	}
	evidence, _ := json.Marshal(decision.Evidence)
	return fmt.Errorf("disk capacity gate blocked: state=%s evidence=%s", decision.State, evidence)
}

// Create materializes a detached worktree at CandidateSHA under TempRoot,
// registers the receipt, and returns a live Probe handle. On any failure
// after the worktree exists, Create attempts a best-effort safe remove of
// a still-clean probe so partial creates do not leak.
func (m *Manager) Create(ctx context.Context, store *Store, req CreateRequest) (*Probe, error) {
	if m == nil {
		return nil, fmt.Errorf("mutationprobe: nil manager")
	}
	if store == nil {
		return nil, fmt.Errorf("mutationprobe: nil store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(req.TaskRef) == "" || strings.TrimSpace(req.Generation) == "" {
		return nil, fmt.Errorf("mutationprobe: task ref and generation are required")
	}
	sha := strings.TrimSpace(req.CandidateSHA)
	if len(sha) != 40 || !isHex(sha) {
		return nil, fmt.Errorf("mutationprobe: candidate sha must be a full 40-character hex SHA")
	}
	if err := m.rejectSharedRoot(); err != nil {
		return nil, err
	}

	probeID := strings.TrimSpace(req.ProbeID)
	if probeID == "" {
		id, err := randomID()
		if err != nil {
			return nil, err
		}
		probeID = id
	}
	if strings.ContainsAny(probeID, `/\`) || filepathIsAbs(probeID) {
		return nil, fmt.Errorf("mutationprobe: probe id must be a portable token, not a path")
	}
	probeName := "herd-mutprobe." + probeID

	tempRoot, err := m.tempRoot()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(tempRoot, 0o755); err != nil {
		return nil, fmt.Errorf("mutationprobe: create temp root: %w", err)
	}
	target := filepath.Join(tempRoot, probeName)
	if err := m.admitDisk(target); err != nil {
		return nil, err
	}
	if _, err := os.Stat(target); err == nil {
		return nil, fmt.Errorf("mutationprobe: target path already exists: %s", probeName)
	}

	m.mu.Lock()
	cmd := m.commandContext()(ctx, "git", "worktree", "add", "--detach", target, sha)
	cmd.Dir = m.OriginRoot
	out, addErr := cmd.CombinedOutput()
	m.mu.Unlock()
	if addErr != nil {
		return nil, fmt.Errorf("mutationprobe: git worktree add: %v (%s)", addErr, strings.TrimSpace(string(out)))
	}

	receipt, err := store.Register(Receipt{
		ProbeID:      probeID,
		TaskRef:      req.TaskRef,
		Generation:   req.Generation,
		CandidateSHA: sha,
		ProbeName:    probeName,
	})
	if err != nil {
		// Registration failed after the worktree was created — attempt a
		// safe remove only if the probe is still clean/disposable.
		_ = m.forceSafeRemoveBestEffort(ctx, target)
		return nil, err
	}
	if err := store.MarkActive(probeID); err != nil {
		_ = m.forceSafeRemoveBestEffort(ctx, target)
		return nil, err
	}

	return &Probe{
		ID:           receipt.ProbeID,
		TaskRef:      receipt.TaskRef,
		Generation:   receipt.Generation,
		CandidateSHA: receipt.CandidateSHA,
		ProbeName:    receipt.ProbeName,
		Path:         target,
	}, nil
}

// PathFor resolves the absolute runtime path of a portable probe name.
func (m *Manager) PathFor(probeName string) (string, error) {
	if probeName == "" || filepathIsAbs(probeName) || strings.Contains(probeName, string(filepath.Separator)) {
		return "", fmt.Errorf("mutationprobe: probe name must be a portable leaf")
	}
	tempRoot, err := m.tempRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(tempRoot, probeName), nil
}

// Classify inspects a live probe directory against the registered candidate
// SHA. It never mutates the worktree.
func (m *Manager) Classify(ctx context.Context, path, candidateSHA string) (Classification, error) {
	c := Classification{Class: ClassUnknown}
	if m == nil {
		c.Reason = "nil manager"
		c.PreserveAction = "keep probe until manager is available"
		return c, fmt.Errorf("mutationprobe: nil manager")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := os.Stat(path); err != nil {
		c.Reason = "probe directory missing"
		c.PreserveAction = "record already-absent; do not invent a removal"
		return c, fmt.Errorf("mutationprobe: probe directory: %w", err)
	}
	head, err := m.revParse(ctx, path, "HEAD")
	if err != nil {
		c.Reason = fmt.Sprintf("HEAD unreadable: %v", err)
		c.PreserveAction = "keep probe until HEAD can be read"
		return c, nil
	}
	c.HEAD = head
	dirty, derr := m.isDirty(ctx, path)
	if derr != nil {
		c.Reason = fmt.Sprintf("status error: %v", derr)
		c.PreserveAction = "keep probe until status can be read"
		return c, nil
	}
	c.Dirty = dirty
	if dirty {
		c.Class = ClassDirty
		c.Reason = "uncommitted changes present"
		c.PreserveAction = "preserve probe byte-for-byte; do not force-remove; recover unique evidence manually"
		return c, nil
	}
	// Unique commits: any commit reachable from HEAD that is not the
	// registered candidate means the probe advanced and may hold unique work.
	if candidateSHA != "" && !strings.EqualFold(head, candidateSHA) {
		// Detached probes stay at candidate unless someone committed.
		// A different HEAD with a clean tree still needs preservation.
		c.Class = ClassUnique
		c.Reason = "HEAD differs from registered candidate SHA"
		c.PreserveAction = "preserve probe; unique tip may hold unmerged evidence"
		return c, nil
	}
	c.Class = ClassDisposable
	c.Reason = "clean disposable probe at registered candidate"
	c.PreserveAction = "safe to remove without --force after absence proof"
	return c, nil
}

// EnsureCleanup is the single compensation path every mutation-probe owner
// should defer. It never force-removes. Dirty/unique/unknown probes are
// preserved and returned as ErrProbePreserved with recovery instructions.
//
// Callers must pass a teardown context that outlives the run's own
// (possibly cancelled) context so cleanup still runs on timeout paths.
func EnsureCleanup(ctx context.Context, store *Store, m *Manager, probeID, expectedTerminalState string) error {
	if store == nil || m == nil {
		return fmt.Errorf("mutationprobe: store and manager are required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	receipt, err := store.Get(probeID)
	if err != nil {
		return err
	}
	if receipt == nil {
		return fmt.Errorf("%w: id=%s", ErrUnknownProbe, probeID)
	}
	if receipt.State == StateRemoved || receipt.State == StatePreserved {
		return nil
	}
	if receipt.State != StateAwaitingCleanup {
		if err := store.MarkAwaitingCleanup(probeID, expectedTerminalState); err != nil {
			return fmt.Errorf("mutationprobe: record terminal outcome for %s: %w", probeID, err)
		}
	}

	path, err := m.PathFor(receipt.ProbeName)
	if err != nil {
		_ = store.MarkPreserved(probeID, ClassUnknown, err.Error())
		return fmt.Errorf("%w: task=%s candidate=%s probe=%s: %v", ErrProbePreserved, receipt.TaskRef, shortSHA(receipt.CandidateSHA), probeID, err)
	}

	class, _ := m.Classify(ctx, path, receipt.CandidateSHA)
	if class.Class != ClassDisposable {
		reason := class.Reason
		if reason == "" {
			reason = string(class.Class)
		}
		_ = store.MarkPreserved(probeID, class.Class, reason)
		return fmt.Errorf("%w: task=%s candidate=%s probe=%s class=%s: %s; %s",
			ErrProbePreserved, receipt.TaskRef, shortSHA(receipt.CandidateSHA), probeID, class.Class, reason, class.PreserveAction)
	}

	if m.SkipCleanup {
		// Test seam: classify succeeded as disposable, but the reap barrier
		// was deliberately removed. Leave the registered worktree in place
		// so non-vacuity tests observe a leak.
		return fmt.Errorf("mutationprobe: cleanup barrier skipped (test seam); probe %s still registered", probeID)
	}

	if m.BeforeRemoveFunc != nil {
		if hookErr := m.BeforeRemoveFunc(ctx, path); hookErr != nil {
			// Re-classify after late mutation.
			class, _ = m.Classify(ctx, path, receipt.CandidateSHA)
			if class.Class != ClassDisposable {
				_ = store.MarkPreserved(probeID, class.Class, class.Reason)
				return fmt.Errorf("%w: task=%s candidate=%s probe=%s: late mutation: %s",
					ErrProbePreserved, receipt.TaskRef, shortSHA(receipt.CandidateSHA), probeID, class.Reason)
			}
			_ = store.MarkPreserved(probeID, ClassUnknown, "removal boundary hook refused action")
			return fmt.Errorf("%w: task=%s candidate=%s probe=%s: removal boundary refused",
				ErrProbePreserved, receipt.TaskRef, shortSHA(receipt.CandidateSHA), probeID)
		}
	}

	if err := m.removeSafely(ctx, path); err != nil {
		_ = store.MarkPreserved(probeID, ClassUnknown, err.Error())
		return fmt.Errorf("%w: task=%s candidate=%s probe=%s: remove failed: %v",
			ErrProbePreserved, receipt.TaskRef, shortSHA(receipt.CandidateSHA), probeID, err)
	}

	absent, aerr := m.absenceProved(ctx, path, receipt.ProbeName)
	if aerr != nil || !absent {
		reason := "absence not proved after remove"
		if aerr != nil {
			reason = aerr.Error()
		}
		_ = store.MarkPreserved(probeID, ClassUnknown, reason)
		return fmt.Errorf("%w: task=%s candidate=%s probe=%s: %s",
			ErrProbePreserved, receipt.TaskRef, shortSHA(receipt.CandidateSHA), probeID, reason)
	}
	return store.MarkRemoved(probeID, true)
}

// WithProbe creates a probe, runs fn, and always runs EnsureCleanup with a
// background teardown context so cancellation of ctx cannot skip reap.
// expectedTerminal is derived from fn's error (success / failed / cancelled).
func WithProbe(ctx context.Context, store *Store, m *Manager, req CreateRequest, fn func(context.Context, *Probe) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	probe, err := m.Create(ctx, store, req)
	if err != nil {
		return err
	}
	terminal := "success"
	defer func() {
		// Teardown must outlive a cancelled run context.
		teardown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if cerr := EnsureCleanup(teardown, store, m, probe.ID, terminal); cerr != nil {
			if err == nil {
				err = cerr
			} else {
				err = fmt.Errorf("%w; cleanup: %v", err, cerr)
			}
		}
	}()
	runErr := fn(ctx, probe)
	if runErr != nil {
		if ctx.Err() != nil {
			terminal = "cancelled"
		} else {
			terminal = "failed"
		}
		return runErr
	}
	if ctx.Err() != nil {
		terminal = "cancelled"
		return ctx.Err()
	}
	return nil
}

// GenerationLive reports whether taskRef's lease/session generation is
// still the active one. A receipt whose generation is no longer live was
// orphaned by a crashed, timed-out, or superseded run.
type GenerationLive func(taskRef, generation string) bool

// ReconcileReport is one sweep's outcome. Slices are sorted by probe ID.
type ReconcileReport struct {
	DryRun      bool     `json:"dry_run"`
	Reclaimed   []string `json:"reclaimed"`
	Preserved   []string `json:"preserved"`
	Skipped     []string `json:"skipped"`
	AlreadyGone []string `json:"already_gone"`
}

// Reconcile sweeps every non-terminal receipt and reclaims probes whose
// owning generation is no longer live. It only acts on receipts this
// store already owns — never on user/build/nested worktrees outside the
// known probe-name prefixes under TempRoot.
func Reconcile(ctx context.Context, store *Store, m *Manager, live GenerationLive) (ReconcileReport, error) {
	var report ReconcileReport
	if store == nil || m == nil {
		return report, fmt.Errorf("mutationprobe: store and manager are required")
	}
	if live == nil {
		return report, fmt.Errorf("mutationprobe: generation liveness probe is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	receipts, err := store.ListNonTerminal()
	if err != nil {
		return report, err
	}
	for _, r := range receipts {
		if live(r.TaskRef, r.Generation) {
			report.Skipped = append(report.Skipped, r.ProbeID)
			continue
		}
		path, perr := m.PathFor(r.ProbeName)
		if perr != nil {
			report.Preserved = append(report.Preserved, r.ProbeID)
			_ = store.MarkPreserved(r.ProbeID, ClassUnknown, perr.Error())
			continue
		}
		if _, err := os.Stat(path); err != nil {
			// Directory already gone — if also unregistered from git, settle as removed.
			registered, _ := m.isRegisteredWorktree(ctx, path)
			if !registered {
				_ = store.MarkRemoved(r.ProbeID, true)
				report.AlreadyGone = append(report.AlreadyGone, r.ProbeID)
				continue
			}
		}
		if err := EnsureCleanup(ctx, store, m, r.ProbeID, "orphaned"); err != nil {
			report.Preserved = append(report.Preserved, r.ProbeID)
			continue
		}
		report.Reclaimed = append(report.Reclaimed, r.ProbeID)
	}
	sortStrings(report.Reclaimed)
	sortStrings(report.Preserved)
	sortStrings(report.Skipped)
	sortStrings(report.AlreadyGone)
	return report, nil
}

// RecoverReport builds a read-only recovery report for a probe leaf name
// (or absolute path's basename). It never deletes. Intended for the
// FAC-83 leaked-probe shape and any preserved receipt.
func RecoverReport(ctx context.Context, store *Store, m *Manager, pathOrName string) (*RecoveryReport, error) {
	if m == nil {
		return nil, fmt.Errorf("mutationprobe: manager is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	leaf := filepath.Base(strings.TrimSpace(pathOrName))
	if leaf == "" || leaf == "." || leaf == string(filepath.Separator) {
		return nil, fmt.Errorf("mutationprobe: path or probe name is required")
	}
	if !isKnownProbeLeaf(leaf) {
		return nil, fmt.Errorf("mutationprobe: %q is not a known mutation-probe leaf; refusing to inspect non-probe worktrees", leaf)
	}
	report := &RecoveryReport{
		ProbeName: leaf,
		PathLeaf:  leaf,
	}
	// Prefer store identity when available.
	if store != nil {
		if all, err := store.ListAll(); err == nil {
			for _, r := range all {
				if r.ProbeName == leaf {
					report.TaskRef = r.TaskRef
					report.CandidateSHA = r.CandidateSHA
					report.Generation = r.Generation
					report.Class = r.Class
					break
				}
			}
		}
	}
	path, err := m.PathFor(leaf)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); err == nil {
		report.DirectoryExists = true
	}
	registered, _ := m.isRegisteredWorktree(ctx, path)
	report.Registered = registered

	if report.DirectoryExists {
		class, _ := m.Classify(ctx, path, report.CandidateSHA)
		report.Class = class.Class
		report.HEAD = class.HEAD
		report.Dirty = class.Dirty
		report.Reason = class.Reason
		report.PreserveAction = class.PreserveAction
	} else if report.Registered {
		report.Class = ClassUnknown
		report.Reason = "directory missing but still registered in origin worktree list"
		report.PreserveAction = "run git worktree prune only after operator confirms no unique evidence remains"
	} else {
		report.Class = ClassDisposable
		report.Reason = "no directory and not registered"
		report.PreserveAction = "already absent; no action"
	}
	// Never auto-delete. Even disposable recovery reports are read-only.
	if report.PreserveAction == "" {
		report.PreserveAction = "read-only recovery report; no automatic deletion"
	} else if report.Class != ClassDisposable {
		// Reinforce the FAC-83 invariant.
		report.PreserveAction += "; no automatic deletion"
	} else {
		report.PreserveAction = "read-only recovery report; no automatic deletion (operator may remove via EnsureCleanup if disposable)"
	}
	return report, nil
}

func (m *Manager) removeSafely(ctx context.Context, path string) error {
	// Never --force. Dirty trees must fail here so they are preserved.
	m.mu.Lock()
	defer m.mu.Unlock()
	cmd := m.commandContext()(ctx, "git", "worktree", "remove", path)
	cmd.Dir = m.OriginRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git worktree remove: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) forceSafeRemoveBestEffort(ctx context.Context, path string) error {
	// Only called on Create failure for a probe we just added and which
	// should still be clean. Still never uses --force.
	return m.removeSafely(ctx, path)
}

func (m *Manager) absenceProved(ctx context.Context, path, probeName string) (bool, error) {
	if _, err := os.Stat(path); err == nil {
		return false, nil
	} else if !os.IsNotExist(err) {
		return false, err
	}
	registered, err := m.isRegisteredWorktree(ctx, path)
	if err != nil {
		return false, err
	}
	if registered {
		return false, nil
	}
	// Also ensure no leaf with this name remains under temp root.
	tempRoot, err := m.tempRoot()
	if err != nil {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(tempRoot, probeName)); err == nil {
		return false, nil
	}
	return true, nil
}

func (m *Manager) isRegisteredWorktree(ctx context.Context, path string) (bool, error) {
	list, err := m.listWorktrees(ctx)
	if err != nil {
		return false, err
	}
	want := normalizePath(path)
	for _, p := range list {
		if normalizePath(p) == want {
			return true, nil
		}
	}
	return false, nil
}

func (m *Manager) listWorktrees(ctx context.Context) ([]string, error) {
	cmd := m.commandContext()(ctx, "git", "worktree", "list", "--porcelain")
	cmd.Dir = m.OriginRoot
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("git worktree list: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "worktree ") {
			paths = append(paths, strings.TrimPrefix(line, "worktree "))
		}
	}
	return paths, nil
}

func (m *Manager) isDirty(ctx context.Context, path string) (bool, error) {
	cmd := m.commandContext()(ctx, "git", "-C", path, "status", "--porcelain")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Errorf("git status: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)) != "", nil
}

func (m *Manager) revParse(ctx context.Context, path, rev string) (string, error) {
	cmd := m.commandContext()(ctx, "git", "-C", path, "rev-parse", rev)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git rev-parse: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func (m *Manager) rejectSharedRoot() error {
	// Origin must itself be a git directory; refuse empty.
	if strings.TrimSpace(m.OriginRoot) == "" {
		return fmt.Errorf("mutationprobe: origin root is required")
	}
	return nil
}

func randomID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("mutationprobe: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func isHex(s string) bool {
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') && (r < 'A' || r > 'F') {
			return false
		}
	}
	return true
}

func shortSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

func isKnownProbeLeaf(leaf string) bool {
	for _, p := range KnownProbePrefixes {
		if strings.HasPrefix(leaf, p) {
			return true
		}
	}
	return false
}

func normalizePath(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return filepath.Clean(p)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved)
	}
	dir := abs
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Clean(filepath.Join(resolved, suffix))
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(abs)
		}
		suffix = filepath.Join(filepath.Base(dir), suffix)
		dir = parent
	}
}

func sortStrings(s []string) {
	if len(s) < 2 {
		return
	}
	// Local sort to avoid importing sort in every call site style.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
