package herdr

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/reviewack"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// The FAC-812 repro: after FAC-807, NativeReviewRetirementOp.RemoveWorktree
// still fails on an authenticated dangling review-surface symlink because
// boundSurfacePath requires the already-deleted pool slot to resolve. The
// installed-runtime evidence (review795-retirement-act-2565.json) shows the
// real state: the pool directory survives, the authenticated slot is absent
// and unregistered, the surface symlink dangles at the exact pool slot path,
// and the failed acts left identity-matched worktree-intent phase records.
// These regressions drive the real native caller (never a fake op) against
// that exact fixture shape.

const (
	danglingFixtureNonce   = "pool-01-1789017234490144000"
	danglingFixtureSlot    = "pool-01"
	danglingFixturePoolRel = ".herd/pool-fac812"
	danglingFixtureSurfRel = ".herd/review-surfaces/review-fac-812-fixture"
	danglingFixtureWtRel   = ".herd/pool-fac812/pool-01"
)

type danglingFixture struct {
	root     string
	op       *NativeReviewRetirementOp
	manifest ReviewRetirementManifest
	surface  string
	slotPath string
	poolJSON string
}

// newDanglingFixture builds the installed-runtime shape: pool directory
// present with only an unrelated released slot registered, the authenticated
// pool-01 slot absent and unregistered, a real dangling surface symlink whose
// raw target is the production-style relative slot path, and the
// identity-matched worktree-intent phase record the failed acts left behind.
// Only Herdr transport calls are faked; every path, symlink, pool state, and
// phase record is real.
func newDanglingFixture(t *testing.T) danglingFixture {
	t.Helper()
	root := t.TempDir()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q")
	if err := os.WriteFile(filepath.Join(root, "base.txt"), []byte("base\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "base.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "base")
	baseSHA := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(root, "cand.txt"), []byte("candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "cand.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=test", "commit", "-qm", "candidate")
	candSHA := runGit("rev-parse", "HEAD")

	// Pool directory survives with only the unrelated released pool-02 slot
	// registered — the exact live shape of .herd/pool-fac786-haiku-0013.
	poolDir := filepath.Join(root, danglingFixturePoolRel)
	if err := os.MkdirAll(filepath.Join(poolDir, "pool-02"), 0o700); err != nil {
		t.Fatal(err)
	}
	poolJSON := `{"version":1,"slots":[{"name":"pool-02","path":"` + danglingFixturePoolRel + `/pool-02","leased_at":"0001-01-01T00:00:00Z","last_release_lease_id":"pool-02-1789036620566064000","last_release_generation":1789036620566064000,"last_release_path":"` + danglingFixturePoolRel + `/pool-02"}]}`
	if err := os.WriteFile(filepath.Join(poolDir, "pool.json"), []byte(poolJSON+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	// The authenticated pool-01 slot is absent and unregistered.
	slotPath := filepath.Join(poolDir, danglingFixtureSlot)

	// A real production-style surface symlink: relative raw target from the
	// surface directory, exactly as herd review --pool creates it, dangling
	// because its slot path is gone.
	surface := filepath.Join(root, danglingFixtureSurfRel)
	if err := os.MkdirAll(filepath.Dir(surface), 0o700); err != nil {
		t.Fatal(err)
	}
	relTarget, err := filepath.Rel(filepath.Dir(surface), slotPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(relTarget, surface); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(slotPath); !os.IsNotExist(err) {
		t.Fatalf("fixture slot path must be absent, got %v", err)
	}

	// Canonical ledger, ack, prompt artifact, manifest artifact, and review
	// ref bind the manifest to durable launch/verdict provenance.
	ledgerPath := filepath.Join(root, ".herd", "review", "ledger.jsonl")
	if err := os.MkdirAll(filepath.Dir(ledgerPath), 0o700); err != nil {
		t.Fatal(err)
	}
	reviewer := "review-fac-812-fixture"
	launchTS := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	manifestTS := time.Now().UTC().Format(time.RFC3339Nano)
	artifactDigest := strings.Repeat("a", 64)
	rows := []string{
		`{"ts":"` + launchTS + `","event":"record","sha":"` + candSHA + `","reviewer":"` + reviewer + `","branch":"refs/herd/reviews/fac-812-fixture","lease":"` + danglingFixtureNonce + `"}`,
		`{"ts":"` + manifestTS + `","event":"verdict","sha":"` + candSHA + `","candidate_sha":"` + candSHA + `","reviewer":"` + reviewer + `","verdict":"PASS","artifact_digest":"` + artifactDigest + `"}`,
	}
	if err := os.WriteFile(ledgerPath, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reviewack.EmitArtifact(root, reviewack.Ack{SHA: candSHA, Reviewer: reviewer, LaunchIdentity: reviewer, ArtifactDigest: artifactDigest}); err != nil {
		t.Fatal(err)
	}
	promptRel := ".herd/review/prompts/fac-812.md"
	promptBody := []byte("review prompt for FAC-812\n")
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(promptRel))), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(promptRel)), promptBody, 0o600); err != nil {
		t.Fatal(err)
	}
	manifestRel := ".herd/review/manifests/pool-01-1789017234490144000.json"
	if err := os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(manifestRel))), 0o700); err != nil {
		t.Fatal(err)
	}
	m := NewReviewRetirementManifest(time.Now(), ReviewRetirementManifest{
		Repository: "example.invalid/fixture", TaskRef: "FAC-812", TaskID: "task-fac-812",
		CandidateSHA: candSHA, BaseSHA: baseSHA, Branch: "refs/herd/reviews/fac-812-fixture", ReviewRef: "refs/herd/reviews/fac-812-fixture",
		Worktree: danglingFixtureWtRel, Pool: danglingFixturePoolRel, Slot: danglingFixtureSlot, LeaseGeneration: 1789017234490144000,
		Workspace: "wK", TabID: "wK:t812", PaneID: "wK:p812", TerminalID: "term_812", SessionID: "ses_812",
		Reviewer: reviewer, ReviewerFamily: "zhipu", ReviewerModel: "litellm/lazer/glm-5.3-flash",
		PromptArtifact: promptRel, PromptDigest: reviewack.ArtifactDigest(promptBody),
		Surface: danglingFixtureSurfRel,
		ManifestArtifact: manifestRel, Generation: danglingFixtureNonce, Nonce: danglingFixtureNonce,
		RecordedAt: manifestTS,
	})
	mJSON, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(manifestRel)), append(mJSON, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("update-ref", m.ReviewRef, candSHA)

	oldRunHerdr := runHerdr
	t.Cleanup(func() { runHerdr = oldRunHerdr })
	runHerdr = func(args ...string) (string, error) {
		if len(args) >= 2 && args[0] == "agent" && args[1] == "list" {
			return `{"result":{"agents":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "tab" && args[1] == "list" {
			return `{"result":{"tabs":[]}}`, nil
		}
		if len(args) >= 2 && args[0] == "pane" && args[1] == "process-info" {
			return `{"error":{"code":"pane_not_found","message":"pane not found"}}`, errors.New("exit status 1")
		}
		return "", errors.New("unexpected fake Herdr command")
	}

	ledger, err := reviewledger.NewReadOnlyReviewLedger(root, ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	op := &NativeReviewRetirementOp{Root: root, RepositoryIdentity: "example.invalid/fixture", Ledger: ledger}
	// The failed acts journaled their identity-matched worktree intent before
	// RemoveWorktree failed — the live journal holds exactly such records.
	if err := op.Journal(m, "worktree-intent"); err != nil {
		t.Fatal(err)
	}
	return danglingFixture{root: root, op: op, manifest: m, surface: surface, slotPath: slotPath, poolJSON: poolJSON}
}

func (f danglingFixture) writePoolState(t *testing.T, slots string) {
	t.Helper()
	body := `{"version":1,"slots":[` + slots + `]}` + "\n"
	if err := os.WriteFile(filepath.Join(f.root, danglingFixturePoolRel, "pool.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (f danglingFixture) requireSurfacePresent(t *testing.T) {
	t.Helper()
	info, err := os.Lstat(f.surface)
	if err != nil {
		t.Fatalf("refused retirement must preserve the surface symlink: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatal("preserved surface is no longer a symlink")
	}
}

func TestNativeRetirementRetiresExactDanglingSurfaceWithAbsentUnregisteredSlot(t *testing.T) {
	f := newDanglingFixture(t)
	res, err := RetireReviewLanesContext(context.Background(), f.op, []ReviewRetirementManifest{f.manifest}, false)
	if err != nil {
		t.Fatalf("exact authenticated dangling surface must retire through the real native caller: %v", err)
	}
	if res.Retired != 1 || res.Failed != 0 || res.Blocked != 0 {
		t.Fatalf("unexpected retirement report: %+v", res)
	}
	if _, err := os.Lstat(f.surface); !os.IsNotExist(err) {
		t.Fatalf("dangling surface must be removed, got %v", err)
	}
	if _, err := os.Stat(f.slotPath); !os.IsNotExist(err) {
		t.Fatalf("retirement must not recreate the absent slot, got %v", err)
	}
	// The surviving unrelated slot record is untouched.
	state, err := os.ReadFile(filepath.Join(f.root, danglingFixturePoolRel, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(state)) != f.poolJSON {
		t.Fatalf("unrelated pool slot record changed: %s", string(state))
	}
	// A second identical transaction is a safe idempotent no-op.
	res2, err := RetireReviewLanesContext(context.Background(), f.op, []ReviewRetirementManifest{f.manifest}, false)
	if err != nil {
		t.Fatalf("second retirement tick failed: %v", err)
	}
	if len(res2.Candidates) != 1 || !res2.Candidates[0].Completed {
		t.Fatalf("second tick must be an idempotent no-op: %+v", res2)
	}
}

// A dangling link into any arbitrary missing path is not ownership proof.
func TestNativeRetirementRefusesDanglingSurfaceWithChangedTarget(t *testing.T) {
	f := newDanglingFixture(t)
	if err := os.Remove(f.surface); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../somewhere-else/missing", f.surface); err != nil {
		t.Fatal(err)
	}
	err := f.op.RemoveWorktree(f.manifest)
	if err == nil || !strings.Contains(err.Error(), "does not target the authenticated pool slot") {
		t.Fatalf("changed dangling target must be refused by the exact-target guard, got %v", err)
	}
	f.requireSurfacePresent(t)
}

// The surface parent chain must stay symlink-free; a symlinked parent can
// escape the repository while keeping an innocent relative name.
func TestNativeRetirementRefusesDanglingSurfaceWithSymlinkedParent(t *testing.T) {
	f := newDanglingFixture(t)
	realDir := filepath.Join(f.root, ".herd", "review-surfaces")
	linkDir := filepath.Join(f.root, ".herd", "review-surfaces-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	m := f.manifest
	m.Surface = ".herd/review-surfaces-link/review-fac-812-fixture"
	err := f.op.RemoveWorktree(m)
	if err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("symlinked surface parent must be refused, got %v", err)
	}
	f.requireSurfacePresent(t)
}

// A phase record from a different generation never authenticates this
// manifest: phase identity is part of the dangling-surface proof.
func TestNativeRetirementRefusesDanglingSurfaceWithWrongGeneration(t *testing.T) {
	f := newDanglingFixture(t)
	foreign := retirementPhaseRecord{
		Generation: "pool-01-0000000000000000001", CandidateSHA: f.manifest.CandidateSHA, Reviewer: f.manifest.Reviewer,
		BindingDigest: f.manifest.BindingDigest, Pool: f.manifest.Pool, Slot: f.manifest.Slot,
		Worktree: f.manifest.Worktree, Nonce: f.manifest.Nonce, LeaseGeneration: f.manifest.LeaseGeneration,
		Phase: "worktree-intent",
	}
	b, err := json.Marshal(foreign)
	if err != nil {
		t.Fatal(err)
	}
	journal := ReviewRetirementPhasesPath(f.root)
	prev, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the manifest's own record out: only the foreign generation remains.
	if err := os.WriteFile(journal, append(b, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.WriteFile(journal, prev, 0o600) })
	err = f.op.RemoveWorktree(f.manifest)
	if err == nil || !strings.Contains(err.Error(), "identity-matched authenticated worktree phase record") {
		t.Fatalf("wrong-generation phase record must not authorize a dangling surface, got %v", err)
	}
	f.requireSurfacePresent(t)
}

// A dangling link to the exact slot path still must not authorize while the
// pool registry keeps the slot.
func TestNativeRetirementRefusesDanglingSurfaceWithRegisteredSlot(t *testing.T) {
	f := newDanglingFixture(t)
	f.writePoolState(t, `{"name":"pool-01","path":"`+danglingFixtureWtRel+`","leased_at":"0001-01-01T00:00:00Z","last_release_lease_id":"`+danglingFixtureNonce+`","last_release_generation":1789017234490144000,"last_release_path":"`+danglingFixtureWtRel+`"}`)
	err := f.op.RemoveWorktree(f.manifest)
	if err == nil || (!strings.Contains(err.Error(), "still registered") && !strings.Contains(err.Error(), "path differs")) {
		t.Fatalf("registered pool slot must refuse a dangling surface, got %v", err)
	}
	f.requireSurfacePresent(t)
	state, readErr := os.ReadFile(filepath.Join(f.root, danglingFixturePoolRel, "pool.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(state), `"pool-01"`) {
		t.Fatal("refused retirement must keep the registered slot record")
	}
}

// If the intended worktree path exists again, the surface resolves through
// the owned-pool path: only the link is ever removed and the replacement
// content is not ours to touch. This pins the "do not delete target content"
// invariant against any future dangling-case shortcut that deletes targets.
func TestNativeRetirementPreservesRecreatedTargetContent(t *testing.T) {
	f := newDanglingFixture(t)
	if err := os.MkdirAll(f.slotPath, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.slotPath, "someone-elses.txt"), []byte("unowned\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := f.op.RemoveWorktree(f.manifest); err != nil {
		t.Fatalf("owned resolving surface must retire without touching target content: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(f.slotPath, "someone-elses.txt")); statErr != nil {
		t.Fatal("retirement destroyed the unowned replacement content")
	}
	state, readErr := os.ReadFile(filepath.Join(f.root, danglingFixturePoolRel, "pool.json"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.TrimSpace(string(state)) != f.poolJSON {
		t.Fatal("replacement-content retirement changed the unrelated pool record")
	}
}

func withFenceProbe(t *testing.T, inject func()) {
	t.Helper()
	prev := reviewSurfaceFenceProbe
	t.Cleanup(func() { reviewSurfaceFenceProbe = prev })
	reviewSurfaceFenceProbe = inject
}

// The final identity fence must refuse when the owned link was swapped for a
// foreign one between authorization and removal.
func TestNativeRetirementFenceRefusesSurfaceReplacementBeforeRemoval(t *testing.T) {
	f := newDanglingFixture(t)
	withFenceProbe(t, func() {
		if err := os.Remove(f.surface); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink("../../somewhere-else/missing", f.surface); err != nil {
			t.Error(err)
		}
	})
	err := f.op.RemoveWorktree(f.manifest)
	if err == nil || !strings.Contains(err.Error(), "identity changed before removal") {
		t.Fatalf("replaced surface must refuse at the final fence, got %v", err)
	}
	f.requireSurfacePresent(t)
}

// A parent swapped for an escaping symlink between authorization and removal
// must refuse at the final fence.
func TestNativeRetirementFenceRefusesParentEscapeBeforeRemoval(t *testing.T) {
	f := newDanglingFixture(t)
	withFenceProbe(t, func() {
		foreign := t.TempDir()
		if err := os.Remove(f.surface); err != nil {
			t.Error(err)
			return
		}
		if err := os.Remove(filepath.Dir(f.surface)); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink(foreign, filepath.Dir(f.surface)); err != nil {
			t.Error(err)
			return
		}
		if err := os.Symlink("../../somewhere-else/missing", f.surface); err != nil {
			t.Error(err)
		}
	})
	err := f.op.RemoveWorktree(f.manifest)
	if err == nil || !strings.Contains(err.Error(), "symlink component") {
		t.Fatalf("escaped surface parent must refuse at the final fence, got %v", err)
	}
	f.requireSurfacePresent(t)
}

// A target recreated between authorization and removal breaks the dangling
// premise the removal was authorized under; the fence must refuse.
func TestNativeRetirementFenceRefusesTargetRecreationBeforeRemoval(t *testing.T) {
	f := newDanglingFixture(t)
	withFenceProbe(t, func() {
		if err := os.MkdirAll(f.slotPath, 0o700); err != nil {
			t.Error(err)
			return
		}
		if err := os.WriteFile(filepath.Join(f.slotPath, "someone-elses.txt"), []byte("unowned\n"), 0o600); err != nil {
			t.Error(err)
		}
	})
	err := f.op.RemoveWorktree(f.manifest)
	if err == nil || !strings.Contains(err.Error(), "resolution changed before removal") {
		t.Fatalf("recreated target must refuse at the final fence, got %v", err)
	}
	f.requireSurfacePresent(t)
	if _, statErr := os.Stat(filepath.Join(f.slotPath, "someone-elses.txt")); statErr != nil {
		t.Fatal("fence refusal destroyed the unowned replacement content")
	}
}
