package harvest

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testReplayRepoID = "repo-v1:sha256:0000000000000000000000000000000000000000000000000000000000000000"

func TestReplayOrderedStableMapping(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-ordered")
	writeFileHarvest(t, wt, "one.txt", "one")
	one := addAndCommitHarvest(t, wt, "one", "one.txt")
	writeFileHarvest(t, wt, "two.txt", "two")
	two := addAndCommitHarvest(t, wt, "two", "two.txt")

	got, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "g1", SourceCommits: []string{one, two}})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if !got.Completed || len(got.Items) != 2 {
		t.Fatalf("unexpected result: %+v", got)
	}
	if got.Items[0].Classification != ReplayAppliedExact || got.Items[1].Classification != ReplayAppliedExact {
		t.Fatalf("ordered exact replay lost: %+v", got.Items)
	}
	if got.Items[0].Order != 0 || got.Items[1].Order != 1 {
		t.Fatalf("order not retained: %+v", got.Items)
	}
	if got.Items[0].Matched == "" || got.Items[0].DestinationHead == "" || got.Items[0].BaseHead == "" {
		t.Fatalf("exact mapping lacks causal heads: %+v", got.Items[0])
	}

	// Replaying the same generation after a restart is an idempotent readback,
	// not a second application.
	again, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "g1", SourceCommits: []string{one, two}})
	if err != nil || len(again.Items) != 2 {
		t.Fatalf("restart readback: result=%+v err=%v", again, err)
	}
}

func TestReplayEmptyAnchorDoesNotAdvanceHead(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-empty")
	empty := addAndCommitHarvest(t, wt, "empty anchor")
	got, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "empty-1", SourceCommits: []string{empty}})
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if got.Items[0].Classification != ReplayEmptyAnchor {
		t.Fatalf("classification=%q", got.Items[0].Classification)
	}
	if got.FinalHead != base {
		t.Fatalf("empty anchor advanced destination: %s != %s", got.FinalHead, base)
	}
}

func TestReplayEmptyAnchorAllowsUnrelatedDestinationContent(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-empty-unrelated")
	empty := addAndCommitHarvest(t, wt, "empty anchor")
	writeFileHarvest(t, root, "unrelated.txt", "destination-only")
	gitInHarvest(t, root, "add", "unrelated.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination-only")
	destination := gitInHarvest(t, root, "rev-parse", "HEAD")
	got, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: destination, Generation: "empty-unrelated", SourceCommits: []string{empty}})
	if err != nil || got.Items[0].Classification != ReplayEmptyAnchor {
		t.Fatalf("unrelated content must not reject empty anchor: result=%+v err=%v", got, err)
	}
	if got.FinalHead != destination {
		t.Fatalf("empty anchor changed destination head")
	}
	_ = base
}

func TestReplayAlreadyPresentAllowsUnrelatedDestinationContent(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-present-unrelated")
	writeFileHarvest(t, wt, "mapped.txt", "mapped")
	source := addAndCommitHarvest(t, wt, "mapped source", "mapped.txt")
	// Apply the same patch through Git, then add a destination-only path.
	gitInHarvest(t, root, "cherry-pick", source)
	writeFileHarvest(t, root, "unrelated.txt", "destination-only")
	gitInHarvest(t, root, "add", "unrelated.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination-only")
	destination := gitInHarvest(t, root, "rev-parse", "HEAD")
	got, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: destination, Generation: "present-unrelated", SourceCommits: []string{source}})
	if err != nil || got.Items[0].Classification != ReplayAlreadyPresent {
		t.Fatalf("unrelated content must not reject present patch: result=%+v err=%v", got, err)
	}
	if got.Items[0].Matched == "" {
		t.Fatal("already-present mapping omitted destination match")
	}
	_ = base
}

func TestReplayRejectsIndistinguishableDuplicatePatchCandidates(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	writeFileHarvest(t, root, "duplicate.txt", "a")
	gitInHarvest(t, root, "add", "duplicate.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "duplicate base")
	wt := createWorktree(t, root, "task/FAC-181-duplicate-patch")
	writeFileHarvest(t, wt, "duplicate.txt", "b")
	source := addAndCommitHarvest(t, wt, "source duplicate patch", "duplicate.txt")
	writeFileHarvest(t, root, "duplicate.txt", "b")
	gitInHarvest(t, root, "add", "duplicate.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination first")
	gitInHarvest(t, root, "revert", "--no-edit", "HEAD")
	writeFileHarvest(t, root, "duplicate.txt", "b")
	gitInHarvest(t, root, "add", "duplicate.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination second")
	expected := gitInHarvest(t, root, "rev-parse", "HEAD")
	_, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: expected, Generation: "duplicate-patch", SourceCommits: []string{source}})
	if !errors.Is(err, ErrReplayBlocked) || !strings.Contains(err.Error(), "ambiguous duplicate") {
		t.Fatalf("duplicate patch candidates must block ambiguously: %v", err)
	}
}

func TestReplayConflictIsRedAndPreservesSequencerEvidence(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	writeFileHarvest(t, root, "shared.txt", "base")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "base shared")
	gitInHarvest(t, root, "push", "-q", "origin", "main")
	wt := createWorktree(t, root, "task/FAC-181-conflict")
	writeFileHarvest(t, wt, "shared.txt", "base\nsource")
	source := addAndCommitHarvest(t, wt, "source conflict", "shared.txt")
	writeFileHarvest(t, root, "shared.txt", "base\ndestination")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination conflict")
	expected := gitInHarvest(t, root, "rev-parse", "HEAD")

	_, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: expected, Generation: "conflict-1", SourceCommits: []string{source}})
	if !errors.Is(err, ErrReplayBlocked) {
		t.Fatalf("conflict must block, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, ".git", "CHERRY_PICK_HEAD")); statErr != nil {
		t.Fatalf("conflict recovery marker missing: %v", statErr)
	}
	stateRel, evidenceRel, pathErr := replayArtifactPaths(ReplayRequest{TaskID: "FAC-181", RepoID: testReplayRepoID, Generation: "conflict-1", ExpectedHead: expected}, digestSources([]string{source}))
	if pathErr != nil {
		t.Fatal(pathErr)
	}
	state, stateErr := loadReplayState(root, stateRel)
	if stateErr != nil {
		t.Fatal(stateErr)
	}
	if state == nil || len(state.Items) != 1 || state.Items[0].Source != source || state.Items[0].Classification != ReplayUnresolved {
		t.Fatalf("conflict checkpoint omitted unresolved source mapping: %+v", state)
	}
	evidence, readErr := os.ReadFile(filepath.Join(root, evidenceRel))
	if readErr != nil || !strings.Contains(string(evidence), "unresolved") {
		t.Fatalf("durable blocked evidence missing: %q %v", evidence, readErr)
	}
	if !strings.Contains(string(evidence), source) {
		t.Fatalf("blocked evidence omitted unresolved source %s: %s", source, evidence)
	}
}

func TestBoundedDiagnosticHashesFullInput(t *testing.T) {
	prefix := strings.Repeat("x", 4096)
	codeA, digestA := boundedDiagnostic(prefix + "a")
	codeB, digestB := boundedDiagnostic(prefix + "b")
	if codeA != codeB {
		t.Fatalf("same diagnostic class changed code: %q != %q", codeA, codeB)
	}
	if digestA == digestB {
		t.Fatal("diagnostics differing after the bound produced the same digest")
	}
	if strings.Contains(digestA, prefix) || strings.Contains(digestB, prefix) {
		t.Fatal("diagnostic digest leaked raw detail")
	}
}

func TestReplayLinkedWorktreeConflictUsesActualGitPath(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	writeFileHarvest(t, root, "shared.txt", "base")
	gitInHarvest(t, root, "add", "shared.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "shared base")
	destination := createWorktree(t, root, "task/FAC-181-linked-destination")
	sourceWT := createWorktree(t, root, "task/FAC-181-linked-source")
	writeFileHarvest(t, sourceWT, "shared.txt", "base\nsource")
	source := addAndCommitHarvest(t, sourceWT, "linked source conflict", "shared.txt")
	writeFileHarvest(t, destination, "shared.txt", "base\ndestination")
	gitInHarvest(t, destination, "add", "shared.txt")
	gitInHarvest(t, destination, "commit", "-q", "-m", "linked destination conflict")
	expected := gitInHarvest(t, destination, "rev-parse", "HEAD")
	_, err := Replay(context.Background(), ReplayRequest{RepoRoot: destination, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: expected, Generation: "linked-conflict", SourceCommits: []string{source}})
	if !errors.Is(err, ErrReplayBlocked) {
		t.Fatalf("linked conflict must block: %v", err)
	}
	path := gitInHarvest(t, destination, "rev-parse", "--git-path", "CHERRY_PICK_HEAD")
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("actual linked gitdir recovery marker missing at %s: %v", path, statErr)
	}
}

func TestReplayRejectsUnmatchedEmptyDelta(t *testing.T) {
	root, remote := setupRepoWithRemote(t)
	// Source adds the final file from the original base.
	wt := createWorktree(t, root, "task/FAC-181-unmatched")
	writeFileHarvest(t, wt, "same.txt", "final")
	source := addAndCommitHarvest(t, wt, "source add", "same.txt")
	// Destination reaches the same tree through two different deltas, so the
	// source application is empty but has no stable patch identity match.
	writeFileHarvest(t, root, "same.txt", "intermediate")
	gitInHarvest(t, root, "add", "same.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination intermediate")
	writeFileHarvest(t, root, "same.txt", "final")
	gitInHarvest(t, root, "add", "same.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "destination final")
	gitInHarvest(t, root, "push", "-q", "origin", "main")
	expected := gitInHarvest(t, root, "rev-parse", "HEAD")
	// Keep remote referenced so the fixture documents the integration boundary.
	_ = remote

	_, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: expected, Generation: "unmatched-1", SourceCommits: []string{source}})
	if !errors.Is(err, ErrReplayBlocked) {
		t.Fatalf("unmatched nonempty delta must block, got %v", err)
	}
}

func TestReplayGenerationFenceRejectsChangedRestart(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-fence")
	writeFileHarvest(t, wt, "fence.txt", "fence")
	source := addAndCommitHarvest(t, wt, "fence", "fence.txt")
	if _, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "g1", SourceCommits: []string{source}}); err != nil {
		t.Fatal(err)
	}
	_, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "g2", SourceCommits: []string{source}})
	if !errors.Is(err, ErrReplayBlocked) {
		t.Fatalf("changed generation must block, got %v", err)
	}
}

func TestReplayCompletedStateRevalidatesDestinationHead(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-complete-drift")
	writeFileHarvest(t, wt, "complete.txt", "complete")
	source := addAndCommitHarvest(t, wt, "complete", "complete.txt")
	req := ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "complete-drift", SourceCommits: []string{source}}
	if _, err := Replay(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	writeFileHarvest(t, root, "drift.txt", "drift")
	gitInHarvest(t, root, "add", "drift.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "drift after completion")
	if _, err := Replay(context.Background(), req); !errors.Is(err, ErrReplayBlocked) {
		t.Fatalf("completed state must reject head drift: %v", err)
	}
}

func TestReplayNextGenerationGetsIndependentDurableState(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-generations")
	writeFileHarvest(t, wt, "generation-a.txt", "a")
	a := addAndCommitHarvest(t, wt, "generation a", "generation-a.txt")
	first := ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "generation-a", SourceCommits: []string{a}}
	firstResult, err := Replay(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	writeFileHarvest(t, wt, "generation-b.txt", "b")
	b := addAndCommitHarvest(t, wt, "generation b", "generation-b.txt")
	second := ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: firstResult.FinalHead, Generation: "generation-b", SourceCommits: []string{b}}
	if _, err := Replay(context.Background(), second); err != nil {
		t.Fatalf("next generation stranded by prior completion: %v", err)
	}
	aSources, _ := resolveSources(context.Background(), root, []string{a})
	bSources, _ := resolveSources(context.Background(), root, []string{b})
	aState, _, _ := replayArtifactPaths(first, digestSources(aSources))
	bState, _, _ := replayArtifactPaths(second, digestSources(bSources))
	if aState == bState {
		t.Fatal("generation state namespace collided")
	}
	if _, err := os.Stat(filepath.Join(root, aState)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, bState)); err != nil {
		t.Fatal(err)
	}
}

func TestReplayRejectsTamperedPersistedMapping(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-tamper")
	writeFileHarvest(t, wt, "tamper.txt", "tamper")
	source := addAndCommitHarvest(t, wt, "tamper", "tamper.txt")
	req := ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "tamper", SourceCommits: []string{source}}
	if _, err := Replay(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveSources(context.Background(), root, []string{source})
	if err != nil {
		t.Fatal(err)
	}
	stateRel, _, err := replayArtifactPaths(req, digestSources(resolved))
	if err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(root, stateRel)
	state, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state = []byte(strings.Replace(string(state), `"matched": "`, `"matched": "tampered-`, 1))
	if err := os.WriteFile(statePath, state, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Replay(context.Background(), req); !errors.Is(err, ErrReplayBlocked) {
		t.Fatalf("tampered mapping must block: %v", err)
	}
}

func TestReplayNULSafeNewlineFilename(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-newline-path")
	name := "line\nname.txt"
	writeFileHarvest(t, wt, name, "newline-safe")
	source := addAndCommitHarvest(t, wt, "newline path", name)
	got, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "newline-path", SourceCommits: []string{source}})
	if err != nil || !got.Completed {
		t.Fatalf("newline filename must replay safely: result=%+v err=%v", got, err)
	}
}

func TestReplaySourceRejectsDuplicateOrderEntries(t *testing.T) {
	root, _ := setupRepoWithRemote(t)
	base := gitInHarvest(t, root, "rev-parse", "HEAD")
	wt := createWorktree(t, root, "task/FAC-181-duplicate")
	writeFileHarvest(t, wt, "dup.txt", "dup")
	source := addAndCommitHarvest(t, wt, "dup", "dup.txt")
	if _, err := Replay(context.Background(), ReplayRequest{RepoRoot: root, TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: base, Generation: "dup", SourceCommits: []string{source, source}}); err == nil {
		t.Fatal("duplicate source order must be rejected")
	}
}

func TestCanonicalRepoIdentityRedactsAndRemainsGenesisBound(t *testing.T) {
	if got, ok := normalizeRemoteIdentity("https://user:secret@Example.COM/org/repo.git?token=bad#fragment"); !ok || got != "https://example.com/org/repo.git" {
		t.Fatalf("credential-bearing remote not normalized: %q %v", got, ok)
	}
	if got, ok := normalizeRemoteIdentity("git@Example.COM:Org/repo.git"); !ok || got != "ssh://example.com/Org/repo.git" {
		t.Fatalf("scp remote not normalized: %q %v", got, ok)
	}
	root, remote := setupRepoWithRemote(t)
	first, err := canonicalRepoIdentity(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	writeFileHarvest(t, root, "later.txt", "later")
	gitInHarvest(t, root, "add", "later.txt")
	gitInHarvest(t, root, "commit", "-q", "-m", "later")
	second, err := canonicalRepoIdentity(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("repo identity drifted after commit: %s != %s", first, second)
	}
	clone := filepath.Join(t.TempDir(), "moved-clone")
	gitInHarvest(t, filepath.Dir(clone), "clone", "-q", remote, clone)
	moved, err := canonicalRepoIdentity(context.Background(), clone)
	if err != nil {
		t.Fatal(err)
	}
	if moved != first {
		t.Fatalf("local remote move changed identity: %s != %s", moved, first)
	}
	other := filepath.Join(t.TempDir(), "other")
	gitInHarvest(t, filepath.Dir(other), "init", "-q", "-b", "main", other)
	writeFileHarvest(t, other, "different.txt", "different genesis")
	gitInHarvest(t, other, "add", "different.txt")
	gitInHarvest(t, other, "commit", "-q", "-m", "different genesis")
	third, err := canonicalRepoIdentity(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if third == first {
		t.Fatal("different repository genesis shared identity")
	}
}

func TestReplayRejectsPoisonedIdentityFields(t *testing.T) {
	base := ReplayRequest{RepoRoot: ".", TaskID: "FAC-181", RepoID: testReplayRepoID, ExpectedHead: strings.Repeat("a", 40), Generation: "g", SourceCommits: []string{"a"}}
	for _, value := range []string{"", "../escape", "line\nfeed", strings.Repeat("x", 300)} {
		req := base
		req.Generation = value
		if err := validateReplayRequest(req); err == nil {
			t.Fatalf("poisoned generation accepted: %q", value)
		}
	}
	badRepo := base
	badRepo.RepoID = "/absolute/secret"
	if err := validateReplayRequest(badRepo); err == nil {
		t.Fatal("absolute repository identity accepted")
	}
}

func TestReplayArchitectureRejectsShellAndBypassCommands(t *testing.T) {
	path := filepath.Join("replay.go")
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{"sh": true, "bash": true, "--continue": true, "--skip": true, "push": true, "merge": true}
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) < 2 {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || (selector.Sel.Name != "Command" && selector.Sel.Name != "CommandContext") {
			return true
		}
		for _, arg := range call.Args {
			lit, ok := arg.(*ast.BasicLit)
			if !ok || lit.Kind.String() != "STRING" {
				continue
			}
			value := strings.Trim(lit.Value, "\"")
			if forbidden[value] {
				t.Errorf("compiled replay authority contains forbidden command argument %q", value)
			}
		}
		return true
	})
}

func TestIntegrationProductionPathUsesReplayAndHasNoLegacyCherryPick(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "integration.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var replayCall, legacyCherryPick bool
	ast.Inspect(file, func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if ok && fn.Name.Name == "cherryPick" {
			legacyCherryPick = true
		}
		// Production path: runMergeGate -> runMergeBatch -> Replay (shared batch/singleton).
		if !ok || (fn.Name.Name != "runMergeGate" && fn.Name.Name != "runMergeBatch") || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(inner ast.Node) bool {
			call, ok := inner.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if ok && ident.Name == "Replay" {
				replayCall = true
			}
			return true
		})
		return true
	})
	if !replayCall {
		t.Fatal("production merge path bypasses compiled Replay authority")
	}
	if legacyCherryPick {
		t.Fatal("legacy cherryPick bypass remains reachable in production package")
	}
}
