package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
)

// authoritativeBoard is a Kaneo-compatible HTTP broker that implements the
// FAC-147 server-side contract (not stock Kaneo): full-schema PUT status with
// X-Herd-Fence + X-Herd-Op, op-id dedupe, stale-fence 409. PATCH is 404.
// Direct clients without fence/op are refused. Client HMAC/description
// receipts are NOT required (audit con62fkm: not provider-native).
type authoritativeBoard struct {
	mu        sync.Mutex
	tasks     map[string]*Task
	comments  map[string][]string
	fenceHigh map[string]int64
	// applied: opID → bound (taskID, fence, kind, body)
	applied map[string]appliedOp
	// effectCount increments only on a first-time apply (not dedupe).
	effectCount atomic.Int32
	// requests counts every mutate HTTP call that reached enforcement.
	requests atomic.Int32
	// rejectUnfenced when false models stock Kaneo (accept status without fence).
	// Default true = FAC-147 enforcing board.
	rejectUnfenced bool
}

type appliedOp struct {
	TaskID string
	Fence  int64
	Kind   string // "status" | "comment"
	Body   string // status value or comment body
}

func newAuthBoard() *authoritativeBoard {
	return &authoritativeBoard{
		tasks:          make(map[string]*Task),
		comments:       make(map[string][]string),
		fenceHigh:      make(map[string]int64),
		applied:        make(map[string]appliedOp),
		rejectUnfenced: true,
	}
}

// enableAtomicFence marks a KaneoProvider as talking to a server that enforces
// fence+op+op-dedupe with status (sandbox/enforcing boards only).
func enableAtomicFence(kp *KaneoProvider) {
	if kp != nil {
		kp.AtomicFenceServer = true
	}
}

func (b *authoritativeBoard) seed(t *Task) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cp := *t
	b.tasks[t.ID] = &cp
}

func (b *authoritativeBoard) Comments(taskID string) []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.comments[taskID]...)
}

func (b *authoritativeBoard) EffectCount() int32  { return b.effectCount.Load() }
func (b *authoritativeBoard) RequestCount() int32 { return b.requests.Load() }

func (b *authoritativeBoard) serve() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// GET /api/activity/{id} — live comment list (ListLiveComments).
		if strings.HasPrefix(r.URL.Path, "/api/activity/") && r.Method == http.MethodGet {
			id := strings.TrimPrefix(r.URL.Path, "/api/activity/")
			b.mu.Lock()
			cmts := append([]string(nil), b.comments[id]...)
			b.mu.Unlock()
			var list []map[string]string
			for _, c := range cmts {
				list = append(list, map[string]string{"type": "comment", "content": c})
			}
			_ = json.NewEncoder(w).Encode(list)
			return
		}

		// GET /api/task/{id}
		if strings.HasPrefix(r.URL.Path, "/api/task/") && r.Method == http.MethodGet && !strings.Contains(r.URL.Path, "/comment") {
			id := strings.TrimPrefix(r.URL.Path, "/api/task/")
			b.mu.Lock()
			t := b.tasks[id]
			b.mu.Unlock()
			if t == nil {
				http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
				return
			}
			// Real Kaneo shape: no herdStatusReceipt field; receipt is in description.
			prio := string(t.Priority)
			if prio == "" {
				prio = "medium"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": t.ID, "ref": t.Ref, "title": t.Title, "status": t.Status,
				"description": t.Description,
				"priority":    prio, "projectId": t.ProjectID, "position": t.Position,
				"labels":    []map[string]string{},
				"createdAt": t.CreatedAt.Format(time.RFC3339),
				"updatedAt": t.UpdatedAt.Format(time.RFC3339Nano),
			})
			return
		}

		// GET /api/task?projectId=
		if r.URL.Path == "/api/task" && r.Method == http.MethodGet {
			b.mu.Lock()
			var list []map[string]any
			for _, t := range b.tasks {
				list = append(list, map[string]any{
					"id": t.ID, "ref": t.Ref, "title": t.Title, "status": t.Status,
					"priority": "medium", "projectId": t.ProjectID, "labels": []map[string]string{},
				})
			}
			b.mu.Unlock()
			_ = json.NewEncoder(w).Encode(list)
			return
		}

		// Mutate: PUT /api/task/{id} (live Kaneo) or POST .../comment.
		// PATCH is intentionally 404 here to match production Kaneo.
		if r.Method == http.MethodPut || r.Method == http.MethodPost {
			b.requests.Add(1)
			fenceHdr := r.Header.Get("X-Herd-Fence")
			opID := r.Header.Get("X-Herd-Op")
			unfencedOK := !b.rejectUnfenced && fenceHdr == "" && opID == ""
			var fence int64
			if !unfencedOK {
				if opID == "" {
					http.Error(w, `{"error":"missing X-Herd-Op"}`, http.StatusBadRequest)
					return
				}
				if fenceHdr == "" {
					http.Error(w, `{"error":"missing X-Herd-Fence"}`, http.StatusBadRequest)
					return
				}
				var err error
				fence, err = strconv.ParseInt(fenceHdr, 10, 64)
				if err != nil {
					http.Error(w, `{"error":"bad fence"}`, http.StatusBadRequest)
					return
				}
			}

			parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
			// api/task/{id} or api/task/{id}/comment
			if len(parts) < 3 || parts[0] != "api" || parts[1] != "task" {
				http.Error(w, `{"error":"bad path"}`, http.StatusNotFound)
				return
			}
			id := parts[2]
			isComment := len(parts) >= 4 && parts[3] == "comment"

			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			asString := func(k string) string {
				if v, ok := body[k]; ok && v != nil {
					return fmt.Sprint(v)
				}
				return ""
			}

			b.mu.Lock()
			defer b.mu.Unlock()

			if !unfencedOK {
				if prev, ok := b.applied[opID]; ok {
					// Idempotent success only when bound metadata matches.
					if prev.TaskID != id || prev.Fence != fence {
						http.Error(w, `{"error":"op metadata mismatch"}`, http.StatusConflict)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"ok":true,"deduped":true}`))
					return
				}

				high := b.fenceHigh[id]
				if fence < high {
					http.Error(w, `{"error":"stale fence"}`, http.StatusConflict)
					return
				}
				if fence > high {
					b.fenceHigh[id] = fence
				}
			}
			t := b.tasks[id]
			if t == nil {
				http.Error(w, `{"error":"missing"}`, http.StatusNotFound)
				return
			}

			if isComment {
				if r.Method != http.MethodPost {
					http.Error(w, `{"error":"comment requires POST"}`, http.StatusMethodNotAllowed)
					return
				}
				cmt := asString("body")
				b.comments[id] = append(b.comments[id], cmt)
				if !unfencedOK {
					b.applied[opID] = appliedOp{TaskID: id, Fence: fence, Kind: "comment", Body: cmt}
				}
			} else {
				if r.Method != http.MethodPut {
					// Match live Kaneo: PATCH is not a status mutate path.
					http.Error(w, `{"error":"status mutate requires PUT"}`, http.StatusNotFound)
					return
				}
				st := NormalizeStatus(asString("status"))
				desc := asString("description")
				// Server-side atomicity: fence+op already checked; op applied map
				// is the op-dedupe receipt. No client HMAC body required.
				t.Status = st
				t.UpdatedAt = time.Now().UTC()
				if desc != "" {
					t.Description = desc
				}
				if title := asString("title"); title != "" {
					t.Title = title
				}
				if prio := asString("priority"); prio != "" {
					t.Priority = Priority(prio)
				}
				if !unfencedOK {
					b.applied[opID] = appliedOp{TaskID: id, Fence: fence, Kind: "status", Body: st}
				}
			}
			b.effectCount.Add(1)
			// Return task-shaped body (live Kaneo PUT response includes fields).
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": t.ID, "title": t.Title, "status": t.Status,
				"description": t.Description, "priority": string(t.Priority),
				"projectId": t.ProjectID, "position": t.Position,
				"updatedAt": t.UpdatedAt.Format(time.RFC3339Nano),
			})
			return
		}
		if r.Method == http.MethodPatch {
			http.Error(w, `{"error":"PATCH not supported; use PUT"}`, http.StatusNotFound)
			return
		}

		http.Error(w, `{"error":"unhandled"}`, http.StatusNotFound)
	}))
}

// kaneoReader adapts KaneoProvider + authoritativeBoard.Comments for
// FencedCAS expectation checks (comment receipt verification).
type kaneoReader struct {
	*KaneoProvider
	board *authoritativeBoard
}

func (k *kaneoReader) Comments(taskID string) []string {
	return k.board.Comments(taskID)
}

// TestKaneoHTTP_AuthoritativeFenceAndOp_TwoLocalStateDirs is the
// non-vacuous production path: real KaneoProvider HTTP → broker that
// enforces X-Herd-Fence/X-Herd-Op; two independent local state dirs share
// the same authoritative service; stale fence and op collision rejected.
func TestKaneoHTTP_AuthoritativeFenceAndOp_TwoLocalStateDirs(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{
		ID: "k1", Ref: "FAC-147k", Title: "k", Status: "to-do",
		ProjectID: "proj", UpdatedAt: now, CreatedAt: now,
	})
	srv := board.serve()
	defer srv.Close()

	kp := NewKaneoProvider(srv.URL, "proj", false)
	enableAtomicFence(kp)
	reader := &kaneoReader{KaneoProvider: kp, board: board}

	// Two independent local state dirs (two hosts / checkouts).
	dirA := t.TempDir()
	dirB := t.TempDir()
	storeA, err := NewSQLiteFenceStore(filepath.Join(dirA, "fences.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	storeB, err := NewSQLiteFenceStore(filepath.Join(dirB, "fences.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()

	casA, err := NewFencedCAS(storeA, reader)
	if err != nil {
		t.Fatal(err)
	}
	casB, err := NewFencedCAS(storeB, reader)
	if err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	rev, err := casA.ReadRevision(ctx, "k1")
	if err != nil {
		t.Fatal(err)
	}

	// A applies gen=1 status via real Kaneo HTTP with fence+op headers.
	opA := "uuid-status-a-1"
	ctxA := WithCASExpectation(ctx, StatusInProgress, "")
	_, err = casA.CompareAndSwap(ctxA, "k1", rev, 1, opA, func(ctx context.Context) error {
		return kp.UpdateStatus(ctx, "k1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("host A status: %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("effects=%d want 1", board.EffectCount())
	}
	// Headers must have been consumed (requests > 0).
	if board.RequestCount() < 1 {
		t.Fatal("authoritative board never received mutate HTTP")
	}

	// B with stale fence=0 against server high=1 must 409 without effect.
	revB, _ := casB.ReadRevision(ctx, "k1")
	var bMutated int
	_, err = casB.CompareAndSwap(ctx, "k1", revB, 0, "uuid-stale-b", func(ctx context.Context) error {
		bMutated++
		return kp.UpdateStatus(ctx, "k1", StatusDone)
	})
	if err == nil || !errors.Is(err, claim.ErrProviderFenceRejected) {
		// Local high-water may reject fence 0 before HTTP if Advance set it;
		// either local or HTTP fence reject is correct.
		if err == nil {
			t.Fatal("stale fence must fail")
		}
	}
	if board.EffectCount() != 1 {
		t.Fatalf("stale fence produced extra effect: %d", board.EffectCount())
	}
	_ = bMutated

	// B with current fence=1 + new op advances status once.
	revB2, _ := casB.ReadRevision(ctx, "k1")
	ctxB := WithCASExpectation(ctx, StatusDone, "")
	_, err = casB.CompareAndSwap(ctxB, "k1", revB2, 1, "uuid-status-b-done", func(ctx context.Context) error {
		return kp.UpdateStatus(ctx, "k1", StatusDone)
	})
	if err != nil {
		t.Fatalf("host B done: %v", err)
	}
	if board.EffectCount() != 2 {
		t.Fatalf("effects=%d want 2 after B", board.EffectCount())
	}

	// Comment via Kaneo HTTP; receipt verified via board.Comments.
	revC, _ := casA.ReadRevision(ctx, "k1")
	body := "dispatch note sandbox"
	ctxC := WithCASExpectation(ctx, "", body)
	_, err = casA.CompareAndSwap(ctxC, "k1", revC, 1, "uuid-comment-1", func(ctx context.Context) error {
		return kp.AddComment(ctx, "k1", body)
	})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	if got := board.Comments("k1"); len(got) != 1 || !strings.HasPrefix(got[0], body) {
		t.Fatalf("comment receipt: %v", got)
	}
	// Idempotent same op: no second comment.
	_, err = casA.CompareAndSwap(ctxC, "k1", revC, 1, "uuid-comment-1", func(ctx context.Context) error {
		return kp.AddComment(ctx, "k1", body)
	})
	if err != nil {
		t.Fatalf("comment retry: %v", err)
	}
	if got := board.Comments("k1"); len(got) != 1 {
		t.Fatalf("duplicate comment: %v", got)
	}
}

// failMarkStore fails MarkApplied once to simulate crash after provider
// commit and before local receipt.
type failMarkStore struct {
	FenceStore
	failOnce atomic.Bool
}

func (f *failMarkStore) MarkApplied(ctx context.Context, rec OpReceipt) error {
	if f.failOnce.CompareAndSwap(true, false) {
		return fmt.Errorf("simulated local receipt crash after provider success")
	}
	return f.FenceStore.MarkApplied(ctx, rec)
}

// TestKaneoHTTP_CrashAfterProviderBeforeLocalReceipt_OneEffect: kill
// exactly after provider commit / before local MarkApplied; restart with
// empty local receipts + same opID yields one effect (server dedupe).
func TestKaneoHTTP_CrashAfterProviderBeforeLocalReceipt_OneEffect(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{
		ID: "crash1", Ref: "FAC-147cr", Title: "c", Status: "to-do",
		ProjectID: "proj", UpdatedAt: now, CreatedAt: now,
	})
	srv := board.serve()
	defer srv.Close()

	kp := NewKaneoProvider(srv.URL, "proj", false)
	enableAtomicFence(kp)
	reader := &kaneoReader{KaneoProvider: kp, board: board}

	// Process A: mutate succeeds on server, local MarkApplied crashes.
	mem := NewMemoryFenceStore()
	failing := &failMarkStore{FenceStore: mem}
	failing.failOnce.Store(true)
	casA, err := NewFencedCAS(failing, reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	rev, _ := casA.ReadRevision(ctx, "crash1")
	opID := "crash-window-op-1"
	ctxA := WithCASExpectation(ctx, StatusInProgress, "")
	_, err = casA.CompareAndSwap(ctxA, "crash1", rev, 1, opID, func(ctx context.Context) error {
		return kp.UpdateStatus(ctx, "crash1", StatusInProgress)
	})
	if err == nil || !errors.Is(err, claim.ErrProviderAmbiguous) {
		t.Fatalf("want ambiguous after local receipt crash, got %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("provider effects=%d want 1", board.EffectCount())
	}

	// Process B: fresh local store (no receipt) — same opID must NOT
	// double-apply at the authoritative broker.
	storeB := NewMemoryFenceStore()
	casB, err := NewFencedCAS(storeB, reader)
	if err != nil {
		t.Fatal(err)
	}
	// Advance local high-water so fence 1 is allowed locally; server still
	// owns applied-ops for opID.
	if _, err := storeB.Advance(ctx, "crash1", 1); err != nil {
		t.Fatal(err)
	}
	cur, _ := casB.ReadRevision(ctx, "crash1")
	var localMutateCalls atomic.Int32
	_, err = casB.CompareAndSwap(ctxA, "crash1", cur, 1, opID, func(ctx context.Context) error {
		localMutateCalls.Add(1)
		return kp.UpdateStatus(ctx, "crash1", StatusInProgress)
	})
	if err != nil {
		t.Fatalf("restart retry: %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("DOUBLE APPLY after crash window: effects=%d", board.EffectCount())
	}
	// Mutate func ran (HTTP hit) but server deduped — one effect.
	if localMutateCalls.Load() != 1 {
		t.Fatalf("expected one HTTP mutate attempt on retry, got %d", localMutateCalls.Load())
	}
	got, _ := kp.GetTask(ctx, "crash1")
	if NormalizeStatus(got.Status) != StatusInProgress {
		t.Fatalf("status=%s", got.Status)
	}
}

// TestKaneoHTTP_OpMetadataMismatch_Rejected: AppliedOp bound to task/fence
// rejects reuse with different metadata.
func TestKaneoHTTP_OpMetadataMismatch_Rejected(t *testing.T) {
	board := newAuthBoard()
	now := time.Now().UTC()
	board.seed(&Task{ID: "m1", Ref: "R1", Title: "t", Status: "to-do", ProjectID: "p", UpdatedAt: now, CreatedAt: now})
	board.seed(&Task{ID: "m2", Ref: "R2", Title: "t", Status: "to-do", ProjectID: "p", UpdatedAt: now, CreatedAt: now})
	srv := board.serve()
	defer srv.Close()
	kp := NewKaneoProvider(srv.URL, "p", false)
	enableAtomicFence(kp)
	reader := &kaneoReader{KaneoProvider: kp, board: board}
	cas, _ := NewFencedCAS(NewMemoryFenceStore(), reader)
	ctx := context.Background()
	rev, _ := cas.ReadRevision(ctx, "m1")
	op := "shared-op-id"
	_, err := cas.CompareAndSwap(WithCASExpectation(ctx, StatusInProgress, ""), "m1", rev, 1, op, func(ctx context.Context) error {
		return kp.UpdateStatus(ctx, "m1", StatusInProgress)
	})
	if err != nil {
		t.Fatal(err)
	}
	// Local receipt binds op→m1/fence1; reuse with different task rejects locally.
	rev2, _ := cas.ReadRevision(ctx, "m2")
	_, err = cas.CompareAndSwap(ctx, "m2", rev2, 1, op, func(ctx context.Context) error {
		return kp.UpdateStatus(ctx, "m2", StatusInProgress)
	})
	if err == nil || !errors.Is(err, claim.ErrProviderFenceRejected) {
		t.Fatalf("want fence rejected on op rebind, got %v", err)
	}
	if board.EffectCount() != 1 {
		t.Fatalf("effects=%d", board.EffectCount())
	}
}

// TestRepoAlias_TwoGitWorktrees_SingleLeaseAuthority creates a real git
// repo with two registered worktrees and proves LeaseKey collides so the
// second acquire conflicts (not independent gen1).
func TestRepoAlias_TwoGitWorktrees_SingleLeaseAuthority(t *testing.T) {
	root := t.TempDir()
	run := func(dir string, args ...string) {
		t.Helper()
		full := append([]string{
			"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
			"-c", "user.email=t@t", "-c", "user.name=t",
		}, args...)
		if len(args) > 0 && args[0] == "commit" {
			full = append([]string{
				"-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false",
				"-c", "user.email=t@t", "-c", "user.name=t",
				"commit", "--no-gpg-sign",
			}, args[1:]...)
		}
		cmd := exec.Command("git", full...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
			"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
			"GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
			"GPG_TTY=", "SSH_AUTH_SOCK=",
		)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run(root, "init", "-b", "main")
	run(root, "config", "commit.gpgsign", "false")
	run(root, "config", "user.email", "t@t")
	run(root, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(root, "README"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(root, "add", "README")
	run(root, "commit", "-m", "init")

	wtA := filepath.Join(t.TempDir(), "wt-a")
	wtB := filepath.Join(t.TempDir(), "wt-b")
	run(root, "worktree", "add", "-b", "branch-a", wtA, "main")
	run(root, "worktree", "add", "-b", "branch-b", wtB, "main")

	// No HERD_ROOT — force git-common-dir path.
	t.Setenv("HERD_ROOT", "")
	t.Setenv("HERD_REPO_ROOT", "")

	k1 := LeaseKey(wtA, "kaneo", "p", "FAC-147al")
	k2 := LeaseKey(wtB, "kaneo", "p", "FAC-147al")
	if k1.Repo != k2.Repo {
		t.Fatalf("worktree lease keys diverged:\n  A=%q\n  B=%q\n want common-dir identity", k1.Repo, k2.Repo)
	}
	if k1.Repo == "" || k1.Repo == "." || k1.Repo == wtA || k1.Repo == wtB {
		t.Fatalf("Repo not canonicalized via git common-dir: %q", k1.Repo)
	}

	mp := NewMemoryProvider()
	mp.AddTask(testTask("alias-1", "FAC-147al", "to-do"))
	// Shared claim dir (production: canonical .herd/claim under main root).
	stack := NewTestStack(t, mp)

	lease1, err := stack.AcquireLease(context.Background(), k1, "wt-a-owner", "worker", "worker")
	if err != nil {
		t.Fatal(err)
	}
	if lease1.Generation < 1 {
		t.Fatalf("generation=%d", lease1.Generation)
	}
	// Second worktree alias MUST conflict — not mint independent gen1.
	_, err = stack.AcquireLease(context.Background(), k2, "wt-b-owner", "worker", "worker")
	if err == nil {
		t.Fatal("second worktree must not acquire concurrent lease on same card")
	}
	if !IsClaimConflict(err) && !strings.Contains(err.Error(), "claim") {
		t.Fatalf("expected claim conflict, got %v", err)
	}
}

// TestLeaseKey_CanonicalRepoAliases is the unit-level companion: Abs-only
// paths without git still must not silently equal; with HERD_ROOT on "."
// they share identity for the process cwd case.
func TestLeaseKey_CanonicalRepoAliases(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HERD_ROOT", root)
	t.Setenv("HERD_REPO_ROOT", root)
	a := LeaseKey(".", "kaneo", "p", "FAC-1")
	c := LeaseKey(".", "kaneo", "p", "FAC-1")
	if a.Repo != c.Repo {
		t.Fatalf("unstable .: %q vs %q", a.Repo, c.Repo)
	}
	if a.Repo == "." {
		t.Fatal("LeaseKey still uses bare . for Repo under HERD_ROOT")
	}
}
