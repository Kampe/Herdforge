package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/resources"
)

// FAC-809: the act-time owner census must run ONCE for the whole set through
// the batched inspector, mapped by exact path, and must refuse every
// missing/unknown/active result. The batch is the point: the serial per-target
// census re-paid the process walk per worktree and starved its deadline.

type reapBatchInspectorFunc struct {
	calls     int
	seenPaths []string
	fn        func(ctx context.Context, paths []string) (map[string]resources.ProcessUsage, error)
}

func (b *reapBatchInspectorFunc) InUseMany(ctx context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
	b.calls++
	b.seenPaths = append(b.seenPaths, paths...)
	return b.fn(ctx, paths)
}

func (b *reapBatchInspectorFunc) InUse(ctx context.Context, path string) (resources.ProcessUsage, error) {
	return resources.ProcessUsage{}, errors.New("serial InUse must not run when the batch census ran")
}

func canonicalPathT(t *testing.T, p string) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(filepath.Dir(p))
	if err != nil {
		t.Fatalf("canonicalize %s: %v", p, err)
	}
	return filepath.Clean(filepath.Join(parent, filepath.Base(p)))
}

func reapBatchWorktree(t *testing.T, root, name string) reapRow {
	t.Helper()
	dir := filepath.Join(root, name)
	runGitT(t, root, "worktree", "add", "-q", "-b", name, dir)
	head := strings.TrimSpace(runGitT(t, dir, "rev-parse", "HEAD"))
	return reapRow{Path: dir, Branch: name, Head: head, Class: "landed"}
}

func TestRetireLandedRunsOneBatchCensusForAllTargets(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	a := reapBatchWorktree(t, root, "wt-a")
	b := reapBatchWorktree(t, root, "wt-b")
	c := reapBatchWorktree(t, root, "wt-c")
	insp := &reapBatchInspectorFunc{fn: func(_ context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
		usage := make(map[string]resources.ProcessUsage, len(paths))
		for _, p := range paths {
			usage[p] = resources.ProcessUsage{}
		}
		return usage, nil
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{a, b, c}, insp)
	if insp.calls != 1 {
		t.Fatalf("exactly one batched census is required for the act set: calls=%d", insp.calls)
	}
	if len(insp.seenPaths) != 3 {
		t.Fatalf("the batch census must receive every target path: %v", insp.seenPaths)
	}
	for _, want := range []string{canonicalPathT(t, a.Path), canonicalPathT(t, b.Path), canonicalPathT(t, c.Path)} {
		found := false
		for _, got := range insp.seenPaths {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("batch census missing exact target %s: %v", want, insp.seenPaths)
		}
	}
	if len(retired) != 3 || len(failed) != 0 {
		t.Fatalf("all-clean census must retire all three: retired=%v failed=%v", retired, failed)
	}
}

func TestRetireLandedBatchRefusesOnBatchErrorAndNeverSerialProbes(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	row := reapBatchWorktree(t, root, "wt-err")
	insp := &reapBatchInspectorFunc{fn: func(context.Context, []string) (map[string]resources.ProcessUsage, error) {
		return nil, errors.New("census process list exhausted the budget")
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{row}, insp)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("batch error must refuse retirement: retired=%v failed=%v", retired, failed)
	}
	if !strings.Contains(failed[0]["error"], "census process list exhausted the budget") {
		t.Fatalf("the refusal must retain the underlying batch cause: %v", failed[0]["error"])
	}
	if insp.calls != 1 {
		t.Fatalf("a failed batch must not fall back to serial scans: calls=%d", insp.calls)
	}
	if !worktreeExists(row.Path) {
		t.Fatal("refused census deleted the worktree")
	}
}

func TestRetireLandedBatchRefusesMissingResultAndMetadataUnavailable(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	missing := reapBatchWorktree(t, root, "wt-missing")
	meta := reapBatchWorktree(t, root, "wt-meta")
	canonicalMeta := canonicalPathT(t, meta.Path)
	insp := &reapBatchInspectorFunc{fn: func(_ context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
		usage := make(map[string]resources.ProcessUsage, len(paths))
		for _, p := range paths {
			if p == canonicalMeta {
				usage[p] = resources.ProcessUsage{MetadataUnavailable: true, MetadataCause: "pid 4711 reference probe: argv read failed"}
			}
		}
		return usage, nil
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{missing, meta}, insp)
	if len(retired) != 0 || len(failed) != 2 {
		t.Fatalf("missing result and unavailable metadata must both refuse: retired=%v failed=%v", retired, failed)
	}
	var missingErr, metaErr string
	for _, f := range failed {
		if f["path"] == missing.Path {
			missingErr = f["error"]
		}
		if f["path"] == meta.Path {
			metaErr = f["error"]
		}
	}
	if !strings.Contains(missingErr, "no batch census result for path") {
		t.Fatalf("missing batch result must be refused by exact path: %v", missingErr)
	}
	if !strings.Contains(metaErr, "pid 4711 reference probe") {
		t.Fatalf("metadata-unavailable refusal must retain the per-pid cause: %v", metaErr)
	}
	if !worktreeExists(missing.Path) || !worktreeExists(meta.Path) {
		t.Fatal("refused census deleted worktrees")
	}
}

func TestRetireLandedBatchRefusesActiveUse(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	row := reapBatchWorktree(t, root, "wt-active")
	insp := &reapBatchInspectorFunc{fn: func(_ context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
		usage := make(map[string]resources.ProcessUsage, len(paths))
		for _, p := range paths {
			usage[p] = resources.ProcessUsage{CWD: true, PIDs: []int{9001}}
		}
		return usage, nil
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{row}, insp)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("active owner must block retirement: retired=%v failed=%v", retired, failed)
	}
	if !strings.Contains(failed[0]["error"], "active use") {
		t.Fatalf("active use must be reported by the act fence: %v", failed[0]["error"])
	}
	if !worktreeExists(row.Path) {
		t.Fatal("active owner refusal removed the worktree")
	}
}

// FAC-809 review finding 1: the real batched inspector keys its result map
// by EvalSymlinks+Clean. A symlinked root must still map census results onto
// the exact raw worktree paths the retirement fences use.
func TestRetireLandedBatchMapsSymlinkedRootOntoRawPaths(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	row := reapBatchWorktree(t, root, "wt-sym")
	linkRoot := filepath.Join(t.TempDir(), "repo-link")
	if err := os.Symlink(root, linkRoot); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	row.Path = filepath.Join(linkRoot, "wt-sym")
	insp := &reapBatchInspectorFunc{fn: func(_ context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
		// Mirror the REAL LSOFProcessInspector.InUseMany keying: resolve
		// every sent path to its canonical identity before answering.
		usage := make(map[string]resources.ProcessUsage, len(paths))
		for _, p := range paths {
			resolved, err := filepath.EvalSymlinks(p)
			if err != nil {
				return nil, err
			}
			usage[filepath.Clean(resolved)] = resources.ProcessUsage{}
		}
		return usage, nil
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{row}, insp)
	if len(retired) != 1 || len(failed) != 0 {
		t.Fatalf("symlinked root census must map onto the raw path and retire: retired=%v failed=%v", retired, failed)
	}
}

// Real-implementation counterpart: pkg/resources.InUseMany keys results by
// canonical path, so a consumer sending raw symlinked paths and looking up
// raw keys would see an empty map. The production consumer canonicalizes.
func TestInUseManyKeysResultsByCanonicalPath(t *testing.T) {
	real := filepath.Join(t.TempDir(), "real-dir")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(t.TempDir(), "link-dir")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	insp := resources.LSOFProcessInspector{Timeout: 2 * time.Second}
	usage, err := insp.InUseMany(context.Background(), []string{link})
	if err != nil {
		t.Fatalf("batch census: %v", err)
	}
	resolved, _ := filepath.EvalSymlinks(link)
	if _, ok := usage[filepath.Clean(resolved)]; !ok {
		t.Fatalf("result must be keyed by the canonical path, got keys: %d", len(usage))
	}
	if _, ok := usage[link]; ok {
		t.Fatalf("raw link key must not be present: %v", usage)
	}
}

// FAC-809 review finding 2: a batch error must fail the census closed even
// when the map carries an apparently clean entry for the target.
func TestRetireLandedBatchRefusesCleanEntryWhenBatchErrored(t *testing.T) {
	root := t.TempDir()
	runGitT(t, root, "init", "-q", "-b", "main", ".")
	runGitT(t, root, "config", "user.email", "t@t")
	runGitT(t, root, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(root, "a"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	runGitT(t, root, "add", ".")
	runGitT(t, root, "commit", "-qm", "base")
	row := reapBatchWorktree(t, root, "wt-clean-on-error")
	insp := &reapBatchInspectorFunc{fn: func(_ context.Context, paths []string) (map[string]resources.ProcessUsage, error) {
		usage := make(map[string]resources.ProcessUsage, len(paths))
		for _, p := range paths {
			usage[p] = resources.ProcessUsage{}
		}
		return usage, errors.New("reference walk stopped at budget")
	}}
	retired, failed := retireLandedWithInspector(root, []reapRow{row}, insp)
	if len(retired) != 0 || len(failed) != 1 {
		t.Fatalf("clean entry under batch error must still refuse: retired=%v failed=%v", retired, failed)
	}
	if !strings.Contains(failed[0]["error"], "reference walk stopped at budget") {
		t.Fatalf("refusal must retain the batch cause: %v", failed[0]["error"])
	}
	if !worktreeExists(row.Path) {
		t.Fatal("refused census deleted the worktree")
	}
}
