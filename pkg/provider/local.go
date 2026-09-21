package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Kampe/Herdforge/pkg/lock"
)

const (
	localBoardDirName = "local-board"
	localStoreName    = "tasks.json"
	localLockWait     = 5 * time.Second
)

// LocalProvider is a persistent, repository-local development board.
// It does not share MemoryProvider's in-process map or its ID scheme.
// It does not implement Kaneo position, StatusReceipt footers, or CAS.
type LocalProvider struct {
	root string
	dir  string
}

type localStore struct {
	NextID   int                 `json:"next_id"`
	Tasks    map[string]*Task    `json:"tasks"`
	Comments map[string][]string `json:"comments"`
}

// NewLocalProvider stores cards under <repoRoot>/.herd/local-board/.
// Symlinked roots, .herd, or store directories are refused. No mkdir here.
func NewLocalProvider(repoRoot string) (*LocalProvider, error) {
	root := strings.TrimSpace(repoRoot)
	if root == "" {
		return nil, fmt.Errorf("local task provider: repository root is required")
	}
	absRoot, err := filepath.Abs(filepath.Clean(root))
	if err != nil {
		return nil, fmt.Errorf("local task provider: resolve root: %w", err)
	}
	if err := refuseSymlinkPath(absRoot); err != nil {
		return nil, fmt.Errorf("local task provider: root: %w", err)
	}
	herdDir := filepath.Join(absRoot, ".herd")
	dir := filepath.Join(herdDir, localBoardDirName)
	if err := refuseEscaping(absRoot, dir); err != nil {
		return nil, err
	}
	if err := refuseSymlinkPath(herdDir); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("local task provider: .herd: %w", err)
	}
	if err := refuseSymlinkPath(dir); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("local task provider: store dir: %w", err)
	}
	return &LocalProvider{root: absRoot, dir: dir}, nil
}

func refuseEscaping(root, candidate string) error {
	rel, err := filepath.Rel(root, candidate)
	if err != nil {
		return fmt.Errorf("local task provider: store path: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("local task provider: store path escapes repository")
	}
	herd := filepath.Join(root, ".herd")
	relHerd, err := filepath.Rel(herd, candidate)
	if err != nil {
		return fmt.Errorf("local task provider: store path: %w", err)
	}
	if relHerd == ".." || strings.HasPrefix(relHerd, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("local task provider: store path escapes .herd")
	}
	return nil
}

func refuseSymlinkPath(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", path)
	}
	return nil
}

func (p *LocalProvider) ensureDir() (created bool, err error) {
	herdDir := filepath.Join(p.root, ".herd")
	if err := refuseSymlinkPath(herdDir); err != nil {
		if os.IsNotExist(err) {
			return false, fmt.Errorf("local task provider: .herd is missing")
		}
		return false, fmt.Errorf("local task provider: .herd: %w", err)
	}
	info, err := os.Lstat(herdDir)
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("local task provider: .herd is not a directory")
	}
	if err := refuseSymlinkPath(p.dir); err != nil {
		if !os.IsNotExist(err) {
			return false, fmt.Errorf("local task provider: store dir: %w", err)
		}
		if err := os.Mkdir(p.dir, 0o755); err != nil {
			return false, fmt.Errorf("local task provider: mkdir: %w", err)
		}
		if err := refuseSymlinkPath(p.dir); err != nil {
			return false, fmt.Errorf("local task provider: store dir after mkdir: %w", err)
		}
		return true, nil
	}
	st, err := os.Lstat(p.dir)
	if err != nil {
		return false, err
	}
	if !st.IsDir() {
		return false, fmt.Errorf("local task provider: store path is not a directory")
	}
	return false, nil
}

func (p *LocalProvider) withStore(ctx context.Context, mutate bool, fn func(*localStore) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	created := false
	if mutate {
		var err error
		created, err = p.ensureDir()
		if err != nil {
			return err
		}
	} else if err := refuseSymlinkPath(p.dir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("local task provider: store dir: %w", err)
	}
	if mutate || dirExists(p.dir) {
		dl := lock.NewDirLock(p.dir)
		if err := dl.Acquire(ctx, localLockWait, "local-board"); err != nil {
			return fmt.Errorf("local task provider: lock: %w", err)
		}
		defer dl.Release()
	}
	st, err := p.load(created)
	if err != nil {
		return err
	}
	if err := fn(st); err != nil {
		return err
	}
	if !mutate {
		return nil
	}
	return p.save(st)
}

func dirExists(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func (p *LocalProvider) load(allowInit bool) (*localStore, error) {
	path := filepath.Join(p.dir, localStoreName)
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			if dirExists(p.dir) && !allowInit {
				return nil, fmt.Errorf("local task provider: store missing after directory init; refusing to reinitialize")
			}
			return &localStore{NextID: 1, Tasks: map[string]*Task{}, Comments: map[string][]string{}}, nil
		}
		return nil, fmt.Errorf("local task provider: lstat store: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("local task provider: store is a symlink")
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("local task provider: store is not a regular file")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, fmt.Errorf("local task provider: open store: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("local task provider: stat store fd: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("local task provider: store fd is not a regular file")
	}
	var st localStore
	dec := json.NewDecoder(f)
	if err := dec.Decode(&st); err != nil {
		return nil, fmt.Errorf("local task provider: parse store: %w", err)
	}
	if err := validateLocalStore(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func validateLocalStore(st *localStore) error {
	if st.Tasks == nil {
		return fmt.Errorf("local task provider: malformed store: tasks is null")
	}
	if st.Comments == nil {
		return fmt.Errorf("local task provider: malformed store: comments is null")
	}
	if st.NextID < 1 {
		return fmt.Errorf("local task provider: malformed store: next_id %d", st.NextID)
	}
	refs := map[string]string{}
	maxLocal := 0
	for key, t := range st.Tasks {
		if t == nil {
			return fmt.Errorf("local task provider: malformed store: nil task %q", key)
		}
		if t.ID != key {
			return fmt.Errorf("local task provider: malformed store: key %q != id %q", key, t.ID)
		}
		if t.Ref != "" {
			if other, ok := refs[t.Ref]; ok {
				return fmt.Errorf("local task provider: malformed store: duplicate ref %q (%s, %s)", t.Ref, other, t.ID)
			}
			refs[t.Ref] = t.ID
		}
		if n, ok := localSeq(t.ID); ok && n > maxLocal {
			maxLocal = n
		}
	}
	if st.NextID <= maxLocal {
		return fmt.Errorf("local task provider: malformed store: next_id %d collides with existing local-%d", st.NextID, maxLocal)
	}
	return nil
}

func localSeq(id string) (int, bool) {
	const pfx = "local-"
	if !strings.HasPrefix(id, pfx) {
		return 0, false
	}
	n, err := strconv.Atoi(id[len(pfx):])
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

func (p *LocalProvider) save(st *localStore) error {
	if err := validateLocalStore(st); err != nil {
		return err
	}
	if err := refuseSymlinkPath(p.dir); err != nil {
		return fmt.Errorf("local task provider: store dir: %w", err)
	}
	path := filepath.Join(p.dir, localStoreName)
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local task provider: store is a symlink")
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("local task provider: store is not a regular file")
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("local task provider: lstat store: %w", err)
	}
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("local task provider: encode store: %w", err)
	}
	tmp, err := os.CreateTemp(p.dir, "tasks-*.json")
	if err != nil {
		return fmt.Errorf("local task provider: temp store: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		_ = tmp.Close()
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := refuseSymlinkPath(tmpName); err != nil {
		return fmt.Errorf("local task provider: temp store: %w", err)
	}
	if _, err := tmp.Write(append(raw, '\n')); err != nil {
		return fmt.Errorf("local task provider: write temp store: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("local task provider: sync temp store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("local task provider: close temp store: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("local task provider: commit store: %w", err)
	}
	cleanup = false
	return nil
}

func (p *LocalProvider) lookup(st *localStore, id string) (*Task, error) {
	if t, ok := st.Tasks[id]; ok {
		cp := *t
		return &cp, nil
	}
	var matches []*Task
	for _, cand := range st.Tasks {
		if cand.Ref == id {
			matches = append(matches, cand)
		}
	}
	if len(matches) > 1 {
		ids := make([]string, 0, len(matches))
		for _, match := range matches {
			ids = append(ids, match.ID)
		}
		sort.Strings(ids)
		return nil, fmt.Errorf("duplicate task ref %q maps to ids %v", id, ids)
	}
	if len(matches) == 1 {
		cp := *matches[0]
		return &cp, nil
	}
	return nil, fmt.Errorf("task not found: %s", id)
}

func (p *LocalProvider) mintID(st *localStore) string {
	for {
		id := fmt.Sprintf("local-%d", st.NextID)
		st.NextID++
		if _, exists := st.Tasks[id]; !exists {
			return id
		}
	}
}

func (p *LocalProvider) CreateTask(ctx context.Context, task *Task) (*Task, error) {
	if task == nil || task.Title == "" || task.ProjectID == "" {
		return nil, fmt.Errorf("create task: title and project are required")
	}
	var out *Task
	err := p.withStore(ctx, true, func(st *localStore) error {
		copy := *task
		if strings.TrimSpace(copy.ID) != "" {
			if _, exists := st.Tasks[copy.ID]; exists {
				return fmt.Errorf("create task: id %q already exists", copy.ID)
			}
		} else {
			copy.ID = p.mintID(st)
		}
		if copy.Ref == "" {
			copy.Ref = copy.ID
		}
		for _, existing := range st.Tasks {
			if existing.Ref == copy.Ref {
				return fmt.Errorf("create task: ref %q already exists as %s", copy.Ref, existing.ID)
			}
		}
		copy.Status = NormalizeStatus(copy.Status)
		if copy.Status == StatusUnknown {
			copy.Status = StatusToDo
		}
		now := time.Now().UTC()
		if copy.CreatedAt.IsZero() {
			copy.CreatedAt = now
		}
		copy.UpdatedAt = now
		stored := copy
		st.Tasks[copy.ID] = &stored
		out = &copy
		return nil
	})
	return out, err
}

func (p *LocalProvider) GetTask(ctx context.Context, id string) (*Task, error) {
	var out *Task
	err := p.withStore(ctx, false, func(st *localStore) error {
		t, err := p.lookup(st, id)
		if err != nil {
			return err
		}
		out = t
		return nil
	})
	return out, err
}

func (p *LocalProvider) ListTasks(ctx context.Context, projectID string, status string) ([]*Task, error) {
	var res []*Task
	err := p.withStore(ctx, false, func(st *localStore) error {
		for _, t := range st.Tasks {
			if projectID != "" && t.ProjectID != projectID {
				continue
			}
			if status != "" && t.Status != status {
				continue
			}
			cp := *t
			res = append(res, &cp)
		}
		sort.SliceStable(res, func(i, j int) bool {
			pi, pj := priorityRank(res[i].Priority), priorityRank(res[j].Priority)
			if pi != pj {
				return pi > pj
			}
			ni, iok := ticketNumber(res[i].Ref)
			nj, jok := ticketNumber(res[j].Ref)
			if iok && jok && ni != nj {
				return ni < nj
			}
			if res[i].Ref != res[j].Ref {
				return res[i].Ref < res[j].Ref
			}
			return res[i].ID < res[j].ID
		})
		return nil
	})
	return res, err
}

func (p *LocalProvider) ClaimTask(ctx context.Context, taskID string, role string) error {
	_ = role
	return p.UpdateStatus(ctx, taskID, StatusInProgress)
}

func (p *LocalProvider) UpdateStatus(ctx context.Context, taskID string, status string) error {
	canonical := NormalizeStatus(status)
	if strings.TrimSpace(status) != "" && canonical == StatusUnknown {
		return fmt.Errorf("local task provider: unknown status %q", status)
	}
	if canonical == StatusUnknown {
		canonical = StatusToDo
	}
	err := p.withStore(ctx, true, func(st *localStore) error {
		cur, err := p.lookup(st, taskID)
		if err != nil {
			return err
		}
		t := st.Tasks[cur.ID]
		t.Status = canonical
		t.UpdatedAt = time.Now().UTC()
		return nil
	})
	if err != nil {
		return err
	}
	got, err := p.GetTask(ctx, taskID)
	if err != nil {
		return fmt.Errorf("local status readback after write: %w", err)
	}
	return VerifyStatusReadback(taskID, canonical, got.Status)
}

func (p *LocalProvider) AddComment(ctx context.Context, taskID string, body string) error {
	return p.withStore(ctx, true, func(st *localStore) error {
		cur, err := p.lookup(st, taskID)
		if err != nil {
			return err
		}
		st.Comments[cur.ID] = append(st.Comments[cur.ID], body)
		t := st.Tasks[cur.ID]
		t.UpdatedAt = time.Now().UTC()
		return nil
	})
}

func (p *LocalProvider) ListComments(ctx context.Context, taskID string) ([]string, error) {
	var out []string
	err := p.withStore(ctx, false, func(st *localStore) error {
		cur, err := p.lookup(st, taskID)
		if err != nil {
			return err
		}
		out = append([]string(nil), st.Comments[cur.ID]...)
		return nil
	})
	return out, err
}

func priorityRank(p Priority) int {
	switch strings.ToLower(string(p)) {
	case string(PriorityUrgent):
		return 4
	case string(PriorityHigh):
		return 3
	case string(PriorityMedium):
		return 2
	case string(PriorityLow):
		return 1
	default:
		return 0
	}
}

func ticketNumber(ref string) (int, bool) {
	i := len(ref)
	for i > 0 && ref[i-1] >= '0' && ref[i-1] <= '9' {
		i--
	}
	if i == len(ref) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[i:])
	if err != nil {
		return 0, false
	}
	return n, true
}
