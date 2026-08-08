package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-160 hermetic CLI integration. A throwaway git repo plus a stub graph tool
// makes the whole gate reproducible without the real code-review-graph CLI or
// the developer's own index.

type testsForOutput struct {
	BaseSHA        string   `json:"base_sha"`
	CandidateSHA   string   `json:"candidate_sha"`
	ChangedPaths   []string `json:"changed_paths"`
	ChangedSymbols []string `json:"changed_symbols"`
	Integrity      struct {
		RepoCommit        string   `json:"repo_commit"`
		ManifestFiles     int      `json:"manifest_files"`
		ManifestDigest    string   `json:"manifest_digest"`
		GraphFiles        int      `json:"graph_files"`
		Queries           []string `json:"queries"`
		UnresolvedTargets []string `json:"unresolved_targets"`
		Rebuilt           bool     `json:"rebuilt"`
		Trusted           bool     `json:"trusted"`
		RejectionReasons  []string `json:"rejection_reasons"`
	} `json:"graph_integrity"`
	Blocked        bool     `json:"blocked"`
	BlockedReasons []string `json:"blocked_reasons"`
	Plan           struct {
		PlannerVersion    string   `json:"planner_version"`
		GraphState        string   `json:"graph_state"`
		Escalated         bool     `json:"escalated"`
		EscalationReasons []string `json:"escalation_reasons"`
		Commands          []struct {
			Stage string   `json:"stage"`
			Argv  []string `json:"argv"`
		} `json:"commands"`
	} `json:"plan"`
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.invalid",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.invalid",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fixtureRepo builds a two-commit repo whose second commit adds an exported
// production symbol inside a directory whose name contains a space.
func fixtureRepo(t *testing.T) (dir, base, candidate string) {
	t.Helper()
	dir = t.TempDir()
	gitIn(t, dir, "init", "-q", "-b", "main")
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "with space"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(rel, body string) {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module fixture\n\ngo 1.22\n")
	write(filepath.Join("pkg", "with space", "thing.go"), "package thing\n\nfunc helper() int { return 1 }\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "base")
	base = gitIn(t, dir, "rev-parse", "HEAD")

	write(filepath.Join("pkg", "with space", "thing.go"),
		"package thing\n\nfunc helper() int { return 1 }\n\n// Exported is new in the candidate.\nfunc Exported() int { return helper() }\n")
	gitIn(t, dir, "add", "-A")
	gitIn(t, dir, "commit", "-q", "-m", "candidate")
	candidate = gitIn(t, dir, "rev-parse", "HEAD")
	return dir, base, candidate
}

// stubGraphTool writes a graph CLI stub reporting `files` File nodes and
// logging every invocation's argv, one NUL-joined line per call.
func stubGraphTool(t *testing.T, files int, testsForHits bool) (path, logPath string) {
	t.Helper()
	dir := t.TempDir()
	path = filepath.Join(dir, "graph-stub")
	logPath = filepath.Join(dir, "calls.log")

	// The stub mirrors the real CLI's wire shape, not a convenient one: query
	// answers are PRETTY-PRINTED with indented result objects and absolute
	// file paths. A single-line stub hid a decoder bug that only a live run
	// surfaced, so the fixture keeps the awkward shape on purpose.
	testsFor := `cat <<EOF
{
  "status": "ok",
  "result_count": 0,
  "results": []
}
EOF`
	if testsForHits {
		testsFor = `cat <<EOF
{
  "status": "ok",
  "result_count": 1,
  "results": [
    {
      "kind": "Test",
      "name": "TestExported",
      "file_path": "$(pwd)/pkg/with space/thing_test.go",
      "is_test": true
    }
  ]
}
EOF`
	}

	script := fmt.Sprintf(`#!/bin/sh
printf '%%s\n' "$*" >> %q
sha=$(git rev-parse HEAD)
case "$1" in
  status)
    printf '{"nodes": 42, "edges": 7, "files": %d, "built_at_commit": "%%s", "current_sha": "%%s"}\n' "$sha" "$sha"
    ;;
  build)
    exit 0
    ;;
  query)
    case "$2" in
      tests_for)
%s
        ;;
      *)
cat <<EOF
{
  "status": "ok",
  "result_count": 0,
  "results": []
}
EOF
        ;;
    esac
    ;;
  *) exit 3 ;;
esac
`, logPath, files, testsFor)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, logPath
}

func runTestsForCLI(t *testing.T, repo string, args ...string) (testsForOutput, string, int) {
	t.Helper()
	cmd := exec.Command(buildHerd(t), append([]string{"tests-for"}, args...)...)
	cmd.Dir = repo
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
	stdout, err := cmd.Output()
	code := 0
	stderr := ""
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
		stderr = string(ee.Stderr)
	} else if err != nil {
		t.Fatalf("run: %v", err)
	}
	var out testsForOutput
	if len(stdout) > 0 {
		if jerr := json.Unmarshal(stdout, &out); jerr != nil {
			t.Fatalf("decode report: %v\n%s", jerr, stdout)
		}
	}
	return out, stderr, code
}

// TestTestsFor_CompleteIndexProducesTargetedPlan proves pkg/graph.Plan has a
// real compiled production caller: the planner_version can only appear in this
// output if the herd binary reached graph.Plan.
func TestTestsFor_CompleteIndexProducesTargetedPlan(t *testing.T) {
	repo, base, cand := fixtureRepo(t)
	// go.mod is not a supported source extension, so the manifest holds exactly
	// the one .go file; a complete index reports at least that many.
	tool, calls := stubGraphTool(t, 1, true)

	out, stderr, code := runTestsForCLI(t, repo, "--graph-tool", tool, base+".."+cand)
	if code != 0 {
		t.Fatalf("exit %d, want 0\nstderr: %s", code, stderr)
	}
	if out.Plan.PlannerVersion == "" {
		t.Fatal("plan is absent: the compiled binary did not reach graph.Plan")
	}
	if !out.Integrity.Trusted || out.Blocked {
		t.Fatalf("complete index must be trusted: %+v", out.Integrity)
	}
	if out.Integrity.Rebuilt {
		t.Fatal("a complete index must not trigger a rebuild")
	}
	if out.BaseSHA != base || out.CandidateSHA != cand {
		t.Fatalf("plan not bound to the exact revision pair: %+v", out)
	}
	if out.Integrity.ManifestFiles != 1 || out.Integrity.ManifestDigest == "" {
		t.Fatalf("evidence must bind the tracked-source manifest: %+v", out.Integrity)
	}
	if out.Plan.GraphState != "available" {
		t.Fatalf("index built at the candidate must be available, got %q", out.Plan.GraphState)
	}
	if len(out.Integrity.Queries) == 0 {
		t.Fatal("query set must be recorded in the evidence")
	}

	// Exported symbol added by the candidate must be detected from the diff.
	if !containsExact(out.ChangedSymbols, "Exported") {
		t.Fatalf("changed exported symbols = %v, want Exported", out.ChangedSymbols)
	}
	// The unexported helper must not be reported as a changed public symbol.
	if containsExact(out.ChangedSymbols, "helper") {
		t.Fatalf("unexported symbol leaked into the public set: %v", out.ChangedSymbols)
	}

	// Directory with a space stays exactly one argv element.
	sawPkg := false
	for _, c := range out.Plan.Commands {
		for _, a := range c.Argv {
			if a == "./pkg/with space" {
				sawPkg = true
			}
			if a == "./pkg/with" || a == "space" {
				t.Fatalf("argv tokenized on the space: %q", c.Argv)
			}
		}
	}
	if !sawPkg {
		t.Fatalf("expected an exact ./pkg/with space argv element: %+v", out.Plan.Commands)
	}

	// No absolute host path may reach the receipt.
	blob, _ := json.Marshal(out)
	if strings.Contains(string(blob), repo) {
		t.Fatalf("absolute host path leaked into the receipt: %s", blob)
	}

	if data, err := os.ReadFile(calls); err != nil {
		t.Fatal(err)
	} else if !strings.Contains(string(data), "status") {
		t.Fatalf("graph tool was not invoked: %q", data)
	}
}

// TestTestsFor_IncompleteIndexBlocksAndBroadens is the live-audit shape: the
// index reports the exact candidate commit but holds fewer files than the
// tracked-source manifest. One bounded rebuild is attempted, parity still
// fails, and the run must exit non-zero with a broadened plan.
func TestTestsFor_IncompleteIndexBlocksAndBroadens(t *testing.T) {
	repo, base, cand := fixtureRepo(t)
	tool, calls := stubGraphTool(t, 0, false) // never covers the manifest

	out, stderr, code := runTestsForCLI(t, repo, "--graph-tool", tool, base+".."+cand)
	if code == 0 {
		t.Fatal("incomplete parity must exit non-zero")
	}
	if !strings.Contains(stderr, "BLOCKED") {
		t.Fatalf("stderr must say BLOCKED, got %q", stderr)
	}
	if out.Integrity.Trusted || !out.Blocked {
		t.Fatalf("incomplete index must not be trusted: %+v", out.Integrity)
	}
	if !out.Integrity.Rebuilt {
		t.Fatal("one bounded full rebuild must be attempted before blocking")
	}
	if !strings.Contains(strings.Join(out.BlockedReasons, "; "), "index incomplete") {
		t.Fatalf("blocked reasons must name the parity failure: %v", out.BlockedReasons)
	}
	if len(out.Integrity.Queries) != 0 {
		t.Fatalf("queries must not run against a rejected index: %v", out.Integrity.Queries)
	}

	// Broadened, not narrowed or empty.
	if !out.Plan.Escalated {
		t.Fatalf("rejected index must broaden the plan: %+v", out.Plan)
	}
	full := false
	for _, c := range out.Plan.Commands {
		if c.Stage == "full" {
			full = true
		}
	}
	if !full {
		t.Fatalf("broadened plan must include the full profile: %+v", out.Plan.Commands)
	}

	// Rebuild is bounded to exactly one attempt.
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	builds := 0
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "build") {
			builds++
		}
	}
	if builds != 1 {
		t.Fatalf("rebuild must be bounded to one attempt, got %d\n%s", builds, data)
	}
}

// TestTestsFor_NoRebuildFlagSkipsTheRebuild pins the escape hatch: with
// --no-rebuild the incomplete index blocks immediately and no build runs.
func TestTestsFor_NoRebuildFlagSkipsTheRebuild(t *testing.T) {
	repo, base, cand := fixtureRepo(t)
	tool, calls := stubGraphTool(t, 0, false)

	out, _, code := runTestsForCLI(t, repo, "--graph-tool", tool, "--no-rebuild", base+".."+cand)
	if code == 0 || !out.Blocked || out.Integrity.Rebuilt {
		t.Fatalf("--no-rebuild must block without rebuilding: code=%d %+v", code, out.Integrity)
	}
	data, _ := os.ReadFile(calls)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "build") {
			t.Fatalf("--no-rebuild must not invoke build:\n%s", data)
		}
	}
}

// TestTestsFor_HostileRefsAreRejectedBeforeAnyToolRuns proves attacker-shaped
// refs stay data: nothing is executed and no graph tool call happens.
func TestTestsFor_HostileRefsAreRejectedBeforeAnyToolRuns(t *testing.T) {
	repo, _, cand := fixtureRepo(t)
	tool, calls := stubGraphTool(t, 1, false)
	sentinel := filepath.Join(t.TempDir(), "pwned")

	hostile := []string{
		"--upload-pack=touch " + sentinel,
		"-x",
		"--output=" + sentinel,
	}
	for i, ref := range hostile {
		ref := ref
		// Bare: the flag parser refuses it before any operational code.
		t.Run(fmt.Sprintf("bare/%d", i), func(t *testing.T) {
			_, stderr, code := runTestsForCLI(t, repo, "--graph-tool", tool, ref+".."+cand)
			if code == 0 {
				t.Fatalf("hostile ref %q must not succeed (stderr %q)", ref, stderr)
			}
		})
		// After --: it reaches the ref validator as literal data and is refused
		// for its shape, never handed to git as an option.
		t.Run(fmt.Sprintf("literal/%d", i), func(t *testing.T) {
			_, stderr, code := runTestsForCLI(t, repo, "--graph-tool", tool, "--", ref+".."+cand)
			if code == 0 {
				t.Fatalf("hostile ref %q must not succeed", ref)
			}
			if !strings.Contains(stderr, "may not start with '-'") {
				t.Fatalf("expected a ref-shape rejection, got %q", stderr)
			}
		})
	}
	if _, err := os.Stat(sentinel); err == nil {
		t.Fatal("hostile ref was executed rather than treated as data")
	}
	if data, err := os.ReadFile(calls); err == nil && len(strings.TrimSpace(string(data))) > 0 {
		t.Fatalf("graph tool ran despite a rejected ref: %q", data)
	}
}

// TestTestsFor_UnknownRefFailsClosed keeps a valid-shaped but nonexistent ref
// from silently planning against an empty change set.
func TestTestsFor_UnknownRefFailsClosed(t *testing.T) {
	repo, _, cand := fixtureRepo(t)
	tool, _ := stubGraphTool(t, 1, false)
	_, stderr, code := runTestsForCLI(t, repo, "--graph-tool", tool, "nonexistentref.."+cand)
	if code == 0 {
		t.Fatal("unknown ref must exit non-zero")
	}
	if !strings.Contains(stderr, "nonexistentref") {
		t.Fatalf("stderr must name the unresolvable ref, got %q", stderr)
	}
}

// notFoundGraphTool stubs a complete index whose tests_for answers not_found —
// the shape a live run returns for a package-level const or var, which has no
// graph node. The run must broaden, not block: blocking here would fail every
// change that adds a constant, and treating it as zero would read as coverage.
func notFoundGraphTool(t *testing.T, files int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph-stub")
	script := fmt.Sprintf(`#!/bin/sh
sha=$(git rev-parse HEAD)
case "$1" in
  status)
    printf '{"nodes": 42, "edges": 7, "files": %d, "built_at_commit": "%%s", "current_sha": "%%s"}\n' "$sha" "$sha"
    ;;
  build) exit 0 ;;
  query)
    case "$2" in
      tests_for)
cat <<EOF
{
  "status": "not_found",
  "summary": "No node found matching '$3'."
}
EOF
        ;;
      *)
cat <<EOF
{
  "status": "ok",
  "result_count": 0,
  "results": []
}
EOF
        ;;
    esac
    ;;
  *) exit 3 ;;
esac
`, files)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestTestsFor_NotFoundTargetBroadensWithoutBlocking(t *testing.T) {
	repo, base, cand := fixtureRepo(t)
	tool := notFoundGraphTool(t, 1)

	out, stderr, code := runTestsForCLI(t, repo, "--graph-tool", tool, base+".."+cand)
	if code != 0 {
		t.Fatalf("a target absent from a complete index must not block: exit %d\nstderr: %s", code, stderr)
	}
	if out.Blocked || !out.Integrity.Trusted {
		t.Fatalf("the index itself is complete and must stay trusted: %+v", out.Integrity)
	}
	if len(out.Integrity.UnresolvedTargets) == 0 {
		t.Fatalf("the unresolvable target must be recorded: %+v", out.Integrity)
	}
	for _, q := range out.Integrity.Queries {
		if strings.HasPrefix(q, "tests_for(") {
			t.Fatalf("not_found must not appear as a zero-result query: %q", q)
		}
	}
	if !out.Plan.Escalated {
		t.Fatalf("underivable coverage must broaden the plan: %+v", out.Plan)
	}
	reasons := strings.Join(out.Plan.EscalationReasons, "; ")
	if !strings.Contains(reasons, "coverage underivable") {
		t.Fatalf("escalation must name the unresolved target, got %q", reasons)
	}
	full := false
	for _, c := range out.Plan.Commands {
		if c.Stage == "full" {
			full = true
		}
	}
	if !full {
		t.Fatalf("broadened plan must include the full profile: %+v", out.Plan.Commands)
	}
}

func TestTestsFor_UsageOnBadArgs(t *testing.T) {
	repo, _, _ := fixtureRepo(t)
	_, stderr, code := runTestsForCLI(t, repo, "just-one-ref")
	if code != 2 {
		t.Fatalf("malformed range must exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "<base>..<candidate>") {
		t.Fatalf("expected usage, got %q", stderr)
	}
}

func containsExact(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func TestParseRevRangeAndRefSafety(t *testing.T) {
	t.Parallel()
	t.Run("range form", func(t *testing.T) {
		t.Parallel()
		b, c, err := parseRevRange([]string{"main..feature"})
		if err != nil || b != "main" || c != "feature" {
			t.Fatalf("b=%q c=%q err=%v", b, c, err)
		}
	})
	t.Run("two args", func(t *testing.T) {
		t.Parallel()
		b, c, err := parseRevRange([]string{"main", "feature"})
		if err != nil || b != "main" || c != "feature" {
			t.Fatalf("b=%q c=%q err=%v", b, c, err)
		}
	})
	t.Run("rejects flag-shaped, empty and control refs", func(t *testing.T) {
		t.Parallel()
		for _, bad := range [][]string{
			{"--upload-pack=x..HEAD"},
			{"..HEAD"},
			{"HEAD.."},
			{"onlyone"},
			{"a", "b", "c"},
			{"HEAD..re\nf"},
		} {
			if _, _, err := parseRevRange(bad); err == nil {
				t.Fatalf("%q must be rejected", bad)
			}
		}
	})
}

func TestSplitEscapingPathsRejectsWorktreeEscape(t *testing.T) {
	t.Parallel()
	keep, rejected := splitEscapingPaths([]string{
		"pkg/with space/thing.go", "../outside.go", "/etc/passwd", "a/../b.go", "..",
	})
	wantKeep := []string{"b.go", "pkg/with space/thing.go"}
	if len(keep) != len(wantKeep) {
		t.Fatalf("keep = %v, want %v", keep, wantKeep)
	}
	for i := range wantKeep {
		if keep[i] != wantKeep[i] {
			t.Fatalf("keep = %v, want %v", keep, wantKeep)
		}
	}
	if len(rejected) != 3 {
		t.Fatalf("rejected = %v, want 3 entries", rejected)
	}
}

func TestChangedExportedGoSymbolsIgnoresTestsAndUnexported(t *testing.T) {
	t.Parallel()
	diff := strings.Join([]string{
		"diff --git a/pkg/a/a.go b/pkg/a/a.go",
		"--- a/pkg/a/a.go",
		"+++ b/pkg/a/a.go",
		"@@ -0,0 +1,4 @@",
		"+func Exported() {}",
		"+func unexported() {}",
		"+type Config struct{}",
		"+var Default = 1",
		"+const internalMax = 3",
		"+func (r Recv) Method() {}",
		"diff --git a/pkg/a/a_test.go b/pkg/a/a_test.go",
		"--- a/pkg/a/a_test.go",
		"+++ b/pkg/a/a_test.go",
		"@@ -0,0 +1,1 @@",
		"+func TestOnlyInTests() {}",
		"diff --git a/README.md b/README.md",
		"--- a/README.md",
		"+++ b/README.md",
		"@@ -0,0 +1,1 @@",
		"+func NotGo() {}",
	}, "\n")

	symbols, byFile := changedExportedGoSymbols(diff)
	want := []string{"Config", "Default", "Exported", "Method"}
	if len(symbols) != len(want) {
		t.Fatalf("symbols = %v, want %v", symbols, want)
	}
	for i := range want {
		if symbols[i] != want[i] {
			t.Fatalf("symbols = %v, want %v", symbols, want)
		}
	}
	if len(byFile["pkg/a/a.go"]) != 4 {
		t.Fatalf("byFile = %v", byFile)
	}
	if _, ok := byFile["pkg/a/a_test.go"]; ok {
		t.Fatal("test files must not contribute production symbols")
	}
	if _, ok := byFile["README.md"]; ok {
		t.Fatal("non-Go files must not contribute symbols")
	}
}

func TestGraphTargetsIsDeterministicAndBounded(t *testing.T) {
	t.Parallel()
	root := "/w/repo"
	byFile := map[string][]string{
		"pkg/b/b.go": {"Beta"},
		"pkg/a/a.go": {"Alpha"},
	}
	targets, truncated := graphTargets(root, []string{"pkg/a/a.go"}, byFile)
	if truncated {
		t.Fatal("small change set must not truncate")
	}
	want := []string{
		"tests_for /w/repo/pkg/a/a.go::Alpha",
		"callers_of /w/repo/pkg/a/a.go::Alpha",
		"tests_for /w/repo/pkg/b/b.go::Beta",
		"callers_of /w/repo/pkg/b/b.go::Beta",
		"importers_of /w/repo/pkg/a/a.go",
	}
	if len(targets) != len(want) {
		t.Fatalf("targets = %+v, want %v", targets, want)
	}
	for i, w := range want {
		got := targets[i].Pattern + " " + targets[i].Target
		if got != w {
			t.Fatalf("target[%d] = %q, want %q", i, got, w)
		}
	}

	// Over the cap the caller is told, so a truncated query set can never read
	// as "the graph found nothing".
	big := map[string][]string{}
	for i := 0; i < maxGraphQueries; i++ {
		big[fmt.Sprintf("pkg/p%03d/x.go", i)] = []string{"Sym"}
	}
	capped, truncated := graphTargets(root, nil, big)
	if !truncated || len(capped) != maxGraphQueries {
		t.Fatalf("expected truncation at %d, got %d truncated=%v", maxGraphQueries, len(capped), truncated)
	}
}
