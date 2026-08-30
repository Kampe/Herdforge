package worktree

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recoveryFixture struct {
	pool      *Pool
	req       ReviewPoolRecoveryRequest
	probes    ReviewPoolRecoveryProbes
	candidate string
	slot      PoolSlot
}

func newRecoveryFixture(t *testing.T) recoveryFixture {
	t.Helper()
	repo := t.TempDir()
	initRepo(t, repo)
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".herd/pool/\n.herd/recovery/\n**/.herd/runtime/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(repo, "add", ".gitignore")
	run(repo, "commit", "-m", "test: ignore runtime authority")
	pool := NewPool(repo, filepath.Join(repo, ".herd", "pool"), 1)
	pool.DefaultBase = "main"
	if err := pool.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := pool.Lease(context.Background(), "review-fac-654-candidate")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(lease.Path, "reviewed.txt"), []byte("reviewed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(lease.Path, "add", "reviewed.txt")
	run(lease.Path, "commit", "-m", "test: unreachable reviewed candidate")
	candidate := run(lease.Path, "rev-parse", "HEAD")
	base := run(repo, "rev-parse", "main")

	evidenceRel := filepath.ToSlash(filepath.Join(".herd", "review", "inbox", candidate+"-review.md"))
	evidence := []byte("---\ntask: FAC-654\nsha: " + candidate + "\nreviewer: review-fac-654-google\nreviewer-family: google\nbuilder-family: openai\nreviewed-head: " + candidate + "\nverdict: PASS\n---\n" + strings.Repeat("independent review evidence confirms the exact candidate and its tests. ", 8) + "\n")
	evidencePath := filepath.Join(repo, filepath.FromSlash(evidenceRel))
	if err := os.MkdirAll(filepath.Dir(evidencePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evidencePath, evidence, 0o600); err != nil {
		t.Fatal(err)
	}
	evidenceDigest := fmt.Sprintf("%x", sha256.Sum256(evidence))
	revision, err := pool.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	host := "review-host-1"
	repoID := "git@example.invalid:Kampe/Herdforge.git"
	projectID := "project-1"
	taskRevision := "task-revision-1"
	req := ReviewPoolRecoveryRequest{
		Version: 1, TransactionID: "fac-663-recovery-1", Repository: repoID,
		ProjectID: projectID, Host: host, PoolRoot: ".herd/pool", StateRevision: revision,
		TaskRef: "FAC-654", TaskID: "task-654", TaskRevision: taskRevision, TaskStatus: "done",
		Slots: []ReviewPoolRecoverySlot{{
			Name: lease.Name, Path: ".herd/pool/pool-01", LeaseID: lease.LeaseID,
			Purpose: lease.Purpose, Head: candidate, Base: base, CandidateSHA: candidate,
			Evidence: []ReviewPoolRecoveryEvidence{{Path: evidenceRel, SHA256: evidenceDigest}},
		}},
	}
	probes := ReviewPoolRecoveryProbes{
		Hostname:     func(context.Context) (string, error) { return host, nil },
		Repository:   func(context.Context) (string, error) { return repoID, nil },
		ProjectID:    func(context.Context) (string, error) { return projectID, nil },
		HolderLive:   func(context.Context, string) (bool, error) { return false, nil },
		OpenFiles:    func(context.Context, string) ([]string, error) { return nil, nil },
		TaskEvidence: func(context.Context, string, string) (string, string, error) { return taskRevision, "done", nil },
		VerdictEvidence: func(_ context.Context, path string) (ReviewPoolVerdictObservation, error) {
			state := filepath.Base(filepath.Dir(path))
			return ReviewPoolVerdictObservation{
				TaskRef: "FAC-654", CandidateSHA: candidate, Verdict: "PASS", Reviewer: "review-fac-654-google",
				ReviewerFamily: "google", BuilderFamily: "openai", State: state,
			}, nil
		},
	}
	return recoveryFixture{pool: pool, req: req, probes: probes, candidate: candidate, slot: *lease}
}

func TestReviewPoolRecoverExactPreservesUnreachableCandidateAndReleasesSlot(t *testing.T) {
	f := newRecoveryFixture(t)
	ctx := context.Background()
	result, err := f.pool.RecoverExact(ctx, f.req, f.probes, true)
	if err != nil {
		t.Fatalf("recover exact: %v", err)
	}
	if result.Idempotent || len(result.Recovered) != 1 || result.Recovered[0] != f.slot.Name {
		t.Fatalf("unexpected result: %+v", result)
	}
	ref := SalvageRefFor("review/FAC-654/" + f.candidate)
	cmd := exec.Command("git", "-C", f.pool.RepoRoot, "rev-parse", ref)
	out, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(out)) != f.candidate {
		t.Fatalf("candidate was not protected by repository salvage authority: out=%q err=%v", out, err)
	}
	slots, err := f.pool.Slots()
	if err != nil {
		t.Fatal(err)
	}
	if len(slots) != 1 || slots[0].LeaseID != "" || slots[0].Purpose != "" {
		t.Fatalf("slot lease not released: %+v", slots)
	}
	if clean, err := gitClean(ctx, f.pool.RepoRoot, f.slot.Path); err != nil || !clean {
		t.Fatalf("recovered slot not clean: clean=%v err=%v", clean, err)
	}
	if got := gitOutput(t, f.slot.Path, "rev-parse", "HEAD"); got != f.req.Slots[0].Base {
		t.Fatalf("slot HEAD=%s want base=%s", got, f.req.Slots[0].Base)
	}
	probeLease, err := f.pool.Lease(ctx, "fac-663-capacity-probe")
	if err != nil {
		t.Fatalf("recovered pool has no clean free capacity: %v", err)
	}
	if err := f.pool.Release(ctx, probeLease.LeaseID); err != nil {
		t.Fatalf("bounded lease/release capacity probe failed: %v", err)
	}
	second, err := f.pool.RecoverExact(ctx, f.req, f.probes, true)
	if err != nil || !second.Idempotent {
		t.Fatalf("idempotent retry: result=%+v err=%v", second, err)
	}
}

func TestReviewPoolRecoverExactFailClosedMatrix(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*recoveryFixture)
		want   string
	}{
		{"wrong host", func(f *recoveryFixture) { f.req.Host = "other-host" }, "host"},
		{"wrong repository", func(f *recoveryFixture) { f.req.Repository = "other/repository" }, "repository"},
		{"wrong project", func(f *recoveryFixture) { f.req.ProjectID = "other-project" }, "project"},
		{"wrong root", func(f *recoveryFixture) { f.req.PoolRoot = ".herd/other-pool" }, "pool root"},
		{"stale state", func(f *recoveryFixture) { f.req.StateRevision = strings.Repeat("0", 64) }, "state revision"},
		{"live holder", func(f *recoveryFixture) {
			f.probes.HolderLive = func(context.Context, string) (bool, error) { return true, nil }
		}, "live holder"},
		{"unknown holder", func(f *recoveryFixture) {
			f.probes.HolderLive = func(context.Context, string) (bool, error) { return false, fmt.Errorf("herdr down") }
		}, "holder"},
		{"open files", func(f *recoveryFixture) {
			f.probes.OpenFiles = func(context.Context, string) ([]string, error) { return []string{"pid=42"}, nil }
		}, "open files"},
		{"task drift", func(f *recoveryFixture) {
			f.probes.TaskEvidence = func(context.Context, string, string) (string, string, error) { return "changed", "done", nil }
		}, "task"},
		{"untransported verdict", func(f *recoveryFixture) {
			f.probes.VerdictEvidence = func(context.Context, string) (ReviewPoolVerdictObservation, error) {
				return ReviewPoolVerdictObservation{TaskRef: "FAC-654", CandidateSHA: f.candidate, Verdict: "PASS", Reviewer: "reviewer", ReviewerFamily: "google", BuilderFamily: "openai", State: "ephemeral"}, nil
			}
		}, "transport"},
		{"same-family verdict", func(f *recoveryFixture) {
			f.probes.VerdictEvidence = func(context.Context, string) (ReviewPoolVerdictObservation, error) {
				return ReviewPoolVerdictObservation{TaskRef: "FAC-654", CandidateSHA: f.candidate, Verdict: "PASS", Reviewer: "reviewer", ReviewerFamily: "openai", BuilderFamily: "openai", State: "inbox"}, nil
			}
		}, "cross-family"},
		{"evidence drift", func(f *recoveryFixture) { f.req.Slots[0].Evidence[0].SHA256 = strings.Repeat("f", 64) }, "evidence"},
		{"candidate ambiguity", func(f *recoveryFixture) { f.req.Slots = append(f.req.Slots, f.req.Slots[0]) }, "duplicate"},
		{"dirty source", func(f *recoveryFixture) {
			_ = os.WriteFile(filepath.Join(f.slot.Path, "dirty.txt"), []byte("dirty"), 0o600)
		}, "dirty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecoveryFixture(t)
			tc.mutate(&f)
			_, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), tc.want) {
				t.Fatalf("err=%v want substring %q", err, tc.want)
			}
			if got := gitOutput(t, f.slot.Path, "rev-parse", "HEAD"); got != f.candidate {
				t.Fatalf("refusal mutated HEAD: got %s want %s", got, f.candidate)
			}
		})
	}
}

func TestReviewPoolRecoverExactRecoversStatePublishedBeforeCompletionJournal(t *testing.T) {
	f := newRecoveryFixture(t)
	if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err != nil {
		t.Fatal(err)
	}
	journal := f.pool.recoveryJournalPath()
	b, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 2 {
		t.Fatalf("journal lines=%d want 2", len(lines))
	}
	// Crash shape: state rename and candidate preservation succeeded, but the
	// append-only completion record never reached disk.
	if err := os.WriteFile(journal, []byte(lines[0]+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true)
	if err != nil || !result.Idempotent {
		t.Fatalf("partial-write retry result=%+v err=%v", result, err)
	}
	b, err = os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(strings.Split(strings.TrimSpace(string(b)), "\n")); got != 2 {
		t.Fatalf("completion journal was not repaired exactly once: lines=%d", got)
	}
}

func TestReviewPoolRecoverExactRefusesCorruptJournalAndConcurrentWinner(t *testing.T) {
	t.Run("partial journal", func(t *testing.T) {
		f := newRecoveryFixture(t)
		if err := os.MkdirAll(filepath.Dir(f.pool.recoveryJournalPath()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f.pool.recoveryJournalPath(), []byte("{partial"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err == nil || !strings.Contains(err.Error(), "partial/corrupt journal") {
			t.Fatalf("corrupt journal accepted: %v", err)
		}
		if got := gitOutput(t, f.slot.Path, "rev-parse", "HEAD"); got != f.candidate {
			t.Fatalf("corrupt journal refusal mutated HEAD: %s", got)
		}
	})
	t.Run("one winner", func(t *testing.T) {
		f := newRecoveryFixture(t)
		if err := os.WriteFile(f.pool.lockPath(), []byte("winner"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err == nil || !strings.Contains(err.Error(), "busy") {
			t.Fatalf("concurrent recovery was not fenced: %v", err)
		}
		if got := gitOutput(t, f.slot.Path, "rev-parse", "HEAD"); got != f.candidate {
			t.Fatalf("concurrent refusal mutated HEAD: %s", got)
		}
	})
}

func TestMutation_RecoveryWithoutCandidatePreservationWouldLoseObject(t *testing.T) {
	f := newRecoveryFixture(t)
	if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err != nil {
		t.Fatal(err)
	}
	ref := SalvageRefFor("review/FAC-654/" + f.candidate)
	if got := gitOutput(t, f.pool.RepoRoot, "rev-parse", ref); got != f.candidate {
		t.Fatalf("mutation: removing salvage publication loses reviewed candidate %s", f.candidate)
	}
	if err := os.Remove(filepath.Join(f.pool.RepoRoot, filepath.FromSlash(f.req.Slots[0].Evidence[0].Path))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err == nil {
		t.Fatal("mutation: idempotent retry accepted after canonical evidence disappeared")
	}
}

func TestMutation_LegacyDeadReclaimCannotBypassExactRecovery(t *testing.T) {
	f := newRecoveryFixture(t)
	f.pool.HolderLive = func(string) bool { return false }
	if _, err := f.pool.ReclaimDead(context.Background()); err == nil || !strings.Contains(err.Error(), "exact recovery") {
		t.Fatalf("legacy dead reclaim bypassed evidence-bound recovery: %v", err)
	}
	if got := gitOutput(t, f.slot.Path, "rev-parse", "HEAD"); got != f.candidate {
		t.Fatalf("legacy reclaim destroyed candidate: got %s want %s", got, f.candidate)
	}
}

func TestReviewPoolRecoverExactPreservesAndRemovesRegisteredNestedWorktree(t *testing.T) {
	f := newRecoveryFixture(t)
	nested := filepath.Join(f.slot.Path, ".herd", "pool", "pool-01")
	cmd := exec.Command("git", "-C", f.pool.RepoRoot, "worktree", "add", "--detach", nested, f.req.Slots[0].Base)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add nested worktree: %v (%s)", err, out)
	}
	if err := os.WriteFile(filepath.Join(nested, "nested.txt"), []byte("nested candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, nested, "add", "nested.txt")
	gitOutput(t, nested, "commit", "-m", "test: nested unique commit")
	nestedHead := gitOutput(t, nested, "rev-parse", "HEAD")
	authorityPath := filepath.Join(nested, ".herd", "runtime", "receipt.json")
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o700); err != nil {
		t.Fatal(err)
	}
	authority := []byte(`{"lease":"nested-authority"}` + "\n")
	if err := os.WriteFile(authorityPath, authority, 0o600); err != nil {
		t.Fatal(err)
	}
	relAuthority, err := filepath.Rel(f.pool.RepoRoot, authorityPath)
	if err != nil {
		t.Fatal(err)
	}
	relNested, err := filepath.Rel(f.pool.RepoRoot, nested)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(authority))
	f.req.Slots[0].Nested = []ReviewPoolRecoveryNested{{
		Path: filepath.ToSlash(relNested), Head: nestedHead, CandidateSHA: nestedHead,
		Authority: []ReviewPoolRecoveryEvidence{{Path: filepath.ToSlash(relAuthority), SHA256: digest}},
	}}
	// Nested registration changed Git topology but not pool.json; refresh only
	// the request digest-bound state revision for clarity.
	f.req.StateRevision, err = f.pool.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err != nil {
		t.Fatalf("recover nested contamination: %v", err)
	}
	if _, err := os.Stat(nested); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registered nested worktree still exists: %v", err)
	}
	archive := filepath.Join(f.pool.RepoRoot, ".herd", "recovery", "review-pool", f.req.TransactionID, "authority", filepath.FromSlash(filepath.ToSlash(relAuthority)))
	if got, err := fileSHA256(archive); err != nil || got != digest {
		t.Fatalf("authority archive readback got=%s err=%v", got, err)
	}
	ref := SalvageRefFor("review/FAC-654/nested/" + nestedHead)
	if got := gitOutput(t, f.pool.RepoRoot, "rev-parse", ref); got != nestedHead {
		t.Fatalf("nested unique commit not preserved: got %s want %s", got, nestedHead)
	}
}

func TestReviewPoolRecoverExactRepairsExplicitUnleasedContaminatedSlot(t *testing.T) {
	f := newRecoveryFixture(t)
	if err := f.pool.withLock(func() error {
		state, err := f.pool.readState()
		if err != nil {
			return err
		}
		state.Slots[0].LeaseID = ""
		state.Slots[0].Purpose = ""
		state.Slots[0].LeasedAt = time.Time{}
		return f.pool.writeState(state)
	}); err != nil {
		t.Fatal(err)
	}
	f.req.Slots[0].LeaseID = ""
	f.req.Slots[0].Purpose = ""
	f.req.Slots[0].Evidence = nil
	var err error
	f.req.StateRevision, err = f.pool.StateRevision()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true); err != nil {
		t.Fatalf("repair exact unleased contamination: %v", err)
	}
	if got := gitOutput(t, f.slot.Path, "rev-parse", "HEAD"); got != f.req.Slots[0].Base {
		t.Fatalf("unleased contaminated slot HEAD=%s want base=%s", got, f.req.Slots[0].Base)
	}
	ref := SalvageRefFor("review/FAC-654/" + f.candidate)
	if got := gitOutput(t, f.pool.RepoRoot, "rev-parse", ref); got != f.candidate {
		t.Fatalf("unleased unique candidate not preserved: %s", got)
	}
}

func TestReviewPoolRecoverExactRefusesUnboundNestedHerdAuthority(t *testing.T) {
	f := newRecoveryFixture(t)
	nested := filepath.Join(f.slot.Path, ".herd", "pool", "pool-01")
	cmd := exec.Command("git", "-C", f.pool.RepoRoot, "worktree", "add", "--detach", nested, f.req.Slots[0].Base)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("add nested worktree: %v (%s)", err, out)
	}
	head := gitOutput(t, nested, "rev-parse", "HEAD")
	authorityPath := filepath.Join(nested, ".herd", "runtime", "receipt.json")
	if err := os.MkdirAll(filepath.Dir(authorityPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorityPath, []byte("authority\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rel, _ := filepath.Rel(f.pool.RepoRoot, nested)
	f.req.Slots[0].Nested = []ReviewPoolRecoveryNested{{Path: filepath.ToSlash(rel), Head: head}}
	_, err := f.pool.RecoverExact(context.Background(), f.req, f.probes, true)
	if err == nil || !strings.Contains(err.Error(), "unbound nested .herd authority") {
		t.Fatalf("unbound nested authority accepted: %v", err)
	}
	if _, statErr := os.Stat(nested); statErr != nil {
		t.Fatalf("refusal removed nested source: %v", statErr)
	}
}

func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
