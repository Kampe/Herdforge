package graph

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// liveAudit* reproduces the 2026-08-03 incident: code-review-graph reported the
// current commit while holding 196 of the 302 eligible files, and answered
// not_found for functions that existed at that SHA.
const (
	liveAuditSHA            = "3e0f7170bc172f14873b198d264b70f01c2f60d3"
	liveAuditIncompleteFile = 196
	liveAuditCompleteFiles  = 302
)

func manifestOfSize(n int) SourceManifest {
	files := make([]string, 0, n)
	for i := 0; i < n; i++ {
		files = append(files, fmt.Sprintf("pkg/p%04d/file.go", i))
	}
	return BuildManifest(files, DefaultSourceExts)
}

func TestBuildManifest_FiltersSortsAndDigests(t *testing.T) {
	t.Parallel()
	m := BuildManifest([]string{
		"pkg/b/b.go", "pkg/a/a.go", "README.md", "pkg/a/a.go",
		"schema/init.sql", "../escape.go", "/abs/x.go", "  ",
	}, DefaultSourceExts)

	want := []string{"pkg/a/a.go", "pkg/b/b.go", "schema/init.sql"}
	if len(m.Files) != len(want) {
		t.Fatalf("files = %v, want %v", m.Files, want)
	}
	for i := range want {
		if m.Files[i] != want[i] {
			t.Fatalf("files = %v, want %v", m.Files, want)
		}
	}
	if m.Digest == "" {
		t.Fatal("digest must be set")
	}
	// Digest is order-independent and content-bound.
	shuffled := BuildManifest([]string{"schema/init.sql", "pkg/b/b.go", "pkg/a/a.go"}, DefaultSourceExts)
	if shuffled.Digest != m.Digest {
		t.Fatal("digest must not depend on input order")
	}
	dropped := BuildManifest([]string{"pkg/a/a.go", "schema/init.sql"}, DefaultSourceExts)
	if dropped.Digest == m.Digest {
		t.Fatal("digest must change when a tracked source file leaves the manifest")
	}
}

// TestCheckIntegrity_RejectsCurrentCommitWithMissingFile is the acceptance
// case: the graph reports the exact right commit but omits one tracked source
// file. Deleting the manifest-parity clause in CheckIntegrity makes this fail.
func TestCheckIntegrity_RejectsCurrentCommitWithMissingFile(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(302)
	status := GraphStatusReport{
		BuiltAtCommit: liveAuditSHA,
		CurrentSHA:    liveAuditSHA,
		Nodes:         3614,
		Edges:         9000,
		Files:         len(m.Files) - 1, // exactly one tracked source file omitted
	}
	reasons := CheckIntegrity(status, liveAuditSHA, m)
	if len(reasons) == 0 {
		t.Fatal("a graph at the right commit missing one tracked source file must be rejected")
	}
	joined := strings.Join(reasons, "; ")
	if !strings.Contains(joined, "index incomplete") {
		t.Fatalf("rejection must name the parity failure, got %q", joined)
	}
	if !strings.Contains(joined, m.Digest) {
		t.Fatalf("rejection must bind the manifest digest, got %q", joined)
	}
}

// TestCheckIntegrity_MutationControl_ParityRemoved pins that the parity clause
// is load-bearing: a checker without it accepts the very index the live audit
// proved untrustworthy, while the real checker rejects it.
func TestCheckIntegrity_MutationControl_ParityRemoved(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(liveAuditCompleteFiles)
	incomplete := GraphStatusReport{
		BuiltAtCommit: liveAuditSHA,
		CurrentSHA:    liveAuditSHA,
		Nodes:         1938,
		Edges:         4000,
		Files:         liveAuditIncompleteFile,
	}

	// Mutant: revision binding only, no manifest parity.
	mutantReasons := func(s GraphStatusReport, commit string) []string {
		if s.BuiltAtCommit != commit {
			return []string{"stale"}
		}
		return nil
	}
	if len(mutantReasons(incomplete, liveAuditSHA)) != 0 {
		t.Fatal("mutant control is miswired; it must accept the 196-file index")
	}
	if len(CheckIntegrity(incomplete, liveAuditSHA, m)) == 0 {
		t.Fatal("the real check must reject the 196-file index the mutant accepts")
	}
}

// TestCheckIntegrity_VerifiedFullRebuildProceeds is the other half of the
// acceptance criterion: a complete index at the exact commit is accepted, so
// the check is not vacuously rejecting everything.
func TestCheckIntegrity_VerifiedFullRebuildProceeds(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(liveAuditCompleteFiles)
	rebuilt := GraphStatusReport{
		BuiltAtCommit: liveAuditSHA,
		CurrentSHA:    liveAuditSHA,
		Nodes:         3614,
		Edges:         9000,
		Files:         liveAuditCompleteFiles,
	}
	if reasons := CheckIntegrity(rebuilt, liveAuditSHA, m); len(reasons) != 0 {
		t.Fatalf("verified full rebuild must proceed, got %v", reasons)
	}
}

func TestCheckIntegrity_RevisionAndEmptinessRejections(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(10)
	full := GraphStatusReport{BuiltAtCommit: "aaa", CurrentSHA: "aaa", Nodes: 100, Files: 10}

	cases := []struct {
		name   string
		status GraphStatusReport
		commit string
		want   string
	}{
		{"unbuilt", GraphStatusReport{Files: 10, Nodes: 1}, "aaa", "no built_at_commit"},
		{"wrong commit", GraphStatusReport{BuiltAtCommit: "bbb", Files: 10, Nodes: 1}, "aaa", "built_at_commit=bbb"},
		{"tree moved", GraphStatusReport{BuiltAtCommit: "aaa", CurrentSHA: "ccc", Files: 10, Nodes: 1}, "aaa", "current_sha=ccc"},
		{"empty commit", full, "", "candidate commit is empty"},
		{"zero nodes", GraphStatusReport{BuiltAtCommit: "aaa", CurrentSHA: "aaa", Files: 10}, "aaa", "zero nodes"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := strings.Join(CheckIntegrity(tc.status, tc.commit, m), "; ")
			if !strings.Contains(got, tc.want) {
				t.Fatalf("reasons %q must contain %q", got, tc.want)
			}
		})
	}
	// Empty manifest is itself a rejection: nothing to prove coverage against.
	if got := strings.Join(CheckIntegrity(full, "aaa", SourceManifest{}), "; "); !strings.Contains(got, "manifest is empty") {
		t.Fatalf("empty manifest must be rejected, got %q", got)
	}
}

// fakeRunner replays canned stdout per argv prefix and records exact argv.
type fakeRunner struct {
	replies map[string]string
	errs    map[string]error
	calls   [][]string
}

func (f *fakeRunner) key(argv []string) string {
	// tool + subcommand (+ pattern for query) identifies the reply slot.
	if len(argv) >= 3 && argv[1] == "query" { //hermetic:allow-argv-position test-fake dispatcher: routes by subcommand name, not a launch contract
		return "query " + argv[2]
	}
	if len(argv) >= 2 {
		return argv[1]
	}
	return ""
}

func (f *fakeRunner) run(_ context.Context, argv []string) ([]byte, error) {
	f.calls = append(f.calls, append([]string(nil), argv...))
	k := f.key(argv)
	if err, ok := f.errs[k]; ok {
		return nil, err
	}
	body, ok := f.replies[k]
	if !ok {
		return nil, fmt.Errorf("no canned reply for %q", k)
	}
	return []byte(body), nil
}

func statusJSON(commit string, files, nodes int) string {
	return fmt.Sprintf(`{"nodes": %d, "edges": 7, "files": %d, "built_at_commit": %q, "current_sha": %q}`,
		nodes, files, commit, commit)
}

func TestCollectEvidence_IncompleteIndexIsNeverDowngradedToNoHits(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(liveAuditCompleteFiles)
	f := &fakeRunner{replies: map[string]string{
		// Both the incremental status and the post-rebuild status stay short.
		"status": statusJSON(liveAuditSHA, liveAuditIncompleteFile, 1938),
		"build":  "",
	}}

	ev, integ, err := CollectEvidence(context.Background(), f.run, EvidenceRequest{
		RepoRoot:     "/repo",
		Commit:       liveAuditSHA,
		Manifest:     m,
		Targets:      []QueryTarget{{Pattern: "tests_for", Target: "/repo/pkg/a/a.go::Exported"}},
		AllowRebuild: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if integ.Trusted {
		t.Fatal("a 196-of-302 index must never be trusted")
	}
	if !integ.Rebuilt {
		t.Fatal("one bounded full rebuild must have been attempted")
	}
	if len(ev.Hits) != 0 || ev.BuiltAtCommit != "" {
		t.Fatalf("untrusted evidence must carry no hits, got %+v", ev)
	}
	if len(integ.Queries) != 0 {
		t.Fatal("queries must not run against an index that failed parity")
	}
	// Exactly one rebuild: status, build, status.
	builds := 0
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == "build" {
			builds++
		}
	}
	if builds != 1 {
		t.Fatalf("rebuild must be bounded to one attempt, got %d", builds)
	}
}

func TestCollectEvidence_RebuildRepairsParityThenQueries(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(3)
	sha := "cafebabe00000000000000000000000000000000"
	calls := 0
	run := func(_ context.Context, argv []string) ([]byte, error) {
		switch argv[1] {
		case "status":
			calls++
			if calls == 1 {
				return []byte(statusJSON(sha, 1, 5)), nil // incomplete
			}
			return []byte(statusJSON(sha, 3, 30)), nil // repaired
		case "build":
			return nil, nil
		case "query":
			return []byte(`{
  "status": "ok",
  "result_count": 1,
  "results": [{"kind": "Test", "name": "TestExported", "file_path": "/repo/pkg/a/a_test.go", "is_test": true}]
}`), nil
		}
		return nil, fmt.Errorf("unexpected %v", argv)
	}

	ev, integ, err := CollectEvidence(context.Background(), run, EvidenceRequest{
		RepoRoot:     "/repo",
		Commit:       sha,
		Manifest:     m,
		Targets:      []QueryTarget{{Pattern: "tests_for", Target: "/repo/pkg/a/a.go::Exported"}},
		AllowRebuild: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !integ.Trusted {
		t.Fatalf("repaired index must be trusted, reasons=%v", integ.RejectionReasons)
	}
	if !integ.Rebuilt || integ.GraphFiles != 3 || integ.ManifestFiles != 3 {
		t.Fatalf("integrity not bound to the repaired index: %+v", integ)
	}
	if len(ev.Hits) != 1 || ev.Hits[0].FilePath != "pkg/a/a_test.go" {
		t.Fatalf("hits must be repo-relative, got %+v", ev.Hits)
	}
	for _, q := range integ.Queries {
		if strings.Contains(q, "/repo/") {
			t.Fatalf("absolute host path leaked into evidence: %q", q)
		}
	}
}

// TestCollectEvidence_AmbiguousQueryIsNotZero pins the fail-closed reading of
// the CLI: `code-review-graph query` exits 0 for ambiguous and not_found alike,
// so a non-ok status must be an error, never an empty hit set.
func TestCollectEvidence_AmbiguousQueryIsNotZero(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(2)
	sha := "deadbeef00000000000000000000000000000000"
	f := &fakeRunner{replies: map[string]string{
		"status":           statusJSON(sha, 2, 20),
		"query tests_for":  `{"status": "ambiguous", "summary": "'Plan' matches 20 node(s)."}`,
		"query callers_of": `{"status": "ok", "result_count": 0, "results": []}`,
	}}
	_, integ, err := CollectEvidence(context.Background(), f.run, EvidenceRequest{
		RepoRoot: "/repo",
		Commit:   sha,
		Manifest: m,
		Targets:  []QueryTarget{{Pattern: "tests_for", Target: "/repo/pkg/a/a.go::Plan"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if integ.Trusted {
		t.Fatal("an ambiguous query result must not be read as zero")
	}
	if !strings.Contains(strings.Join(integ.RejectionReasons, "; "), "ambiguous") {
		t.Fatalf("rejection must name the ambiguity, got %v", integ.RejectionReasons)
	}
}

// TestCollectEvidence_NotFoundIsUnresolvedNotZero pins the third status the
// CLI can return. A package-level const or var has no graph node, so a
// complete index answers not_found. That is authoritative for "no graph
// identity" and therefore says nothing about coverage: it must be recorded as
// unresolved and never counted as a zero-result query, which would read as
// proven coverage.
func TestCollectEvidence_NotFoundIsUnresolvedNotZero(t *testing.T) {
	t.Parallel()
	m := manifestOfSize(2)
	sha := "feedface00000000000000000000000000000000"
	f := &fakeRunner{replies: map[string]string{
		"status":          statusJSON(sha, 2, 20),
		"query tests_for": `{"status": "not_found", "summary": "No node found matching '/repo/pkg/a/a.go::DefaultTool'."}`,
		"query callers_of": `{
  "status": "ok",
  "result_count": 1,
  "results": [
    {"kind": "Function", "name": "Caller", "file_path": "/repo/pkg/b/b.go"}
  ]
}`,
	}}
	ev, integ, err := CollectEvidence(context.Background(), f.run, EvidenceRequest{
		RepoRoot: "/repo",
		Commit:   sha,
		Manifest: m,
		Targets: []QueryTarget{
			{Pattern: "tests_for", Target: "/repo/pkg/a/a.go::DefaultTool"},
			{Pattern: "callers_of", Target: "/repo/pkg/a/a.go::Exported"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A complete index is still trusted; the run must not hard-fail.
	if !integ.Trusted {
		t.Fatalf("not_found against a complete index must not reject the run: %v", integ.RejectionReasons)
	}
	if len(integ.UnresolvedTargets) != 1 || !strings.Contains(integ.UnresolvedTargets[0], "pkg/a/a.go::DefaultTool") {
		t.Fatalf("unresolved target not recorded: %+v", integ.UnresolvedTargets)
	}
	for _, q := range integ.Queries {
		if strings.Contains(q, "DefaultTool") {
			t.Fatalf("not_found must not be recorded as a zero-result query: %q", q)
		}
	}
	// The resolvable query still contributes its evidence.
	if len(ev.Hits) != 1 || ev.Hits[0].FilePath != "pkg/b/b.go" {
		t.Fatalf("resolvable queries must still yield hits: %+v", ev.Hits)
	}
	if len(integ.Queries) != 1 {
		t.Fatalf("exactly one query answered ok, got %v", integ.Queries)
	}
}

func TestCollectEvidence_StatusFailureRejectsAndHidesHostPaths(t *testing.T) {
	t.Parallel()
	f := &fakeRunner{errs: map[string]error{
		"status": errors.New("code-review-graph: no such database under /repo/.crg"),
	}}
	_, integ, err := CollectEvidence(context.Background(), f.run, EvidenceRequest{
		RepoRoot: "/repo",
		Commit:   "abc",
		Manifest: manifestOfSize(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if integ.Trusted {
		t.Fatal("a failed status must not be trusted")
	}
	got := strings.Join(integ.RejectionReasons, "; ")
	if strings.Contains(got, "/repo/") {
		t.Fatalf("absolute host path leaked into rejection reason: %q", got)
	}
}

func TestCollectEvidence_ExactArgvNeverTokenized(t *testing.T) {
	t.Parallel()
	root := "/tmp/work dir/repo"
	sha := "abcabc0000000000000000000000000000000000"
	f := &fakeRunner{replies: map[string]string{
		"status":          statusJSON(sha, 1, 10),
		"query tests_for": `{"status": "ok", "result_count": 0, "results": []}`,
	}}
	target := root + "/pkg/with space/a.go::Exported"
	if _, _, err := CollectEvidence(context.Background(), f.run, EvidenceRequest{
		RepoRoot: root,
		Commit:   sha,
		Manifest: BuildManifest([]string{"pkg/with space/a.go"}, DefaultSourceExts),
		Targets:  []QueryTarget{{Pattern: "tests_for", Target: target}},
	}); err != nil {
		t.Fatal(err)
	}
	var queryCall []string
	for _, c := range f.calls {
		if len(c) > 1 && c[1] == "query" {
			queryCall = c
		}
	}
	if queryCall == nil {
		t.Fatal("no query call recorded")
	}
	want := []string{DefaultGraphTool, "query", "tests_for", target, "--repo", root}
	if len(queryCall) != len(want) {
		t.Fatalf("argv = %q, want %q", queryCall, want)
	}
	for i := range want {
		if queryCall[i] != want[i] {
			t.Fatalf("argv = %q, want %q", queryCall, want)
		}
	}
}

func TestCollectEvidence_NilRunnerIsAnError(t *testing.T) {
	t.Parallel()
	if _, _, err := CollectEvidence(context.Background(), nil, EvidenceRequest{}); err == nil {
		t.Fatal("nil runner must error")
	}
}

func TestLastJSONObject_PrettyAndNoisy(t *testing.T) {
	t.Parallel()
	// Pretty-printed object preceded by tool notices on the same stream.
	raw := []byte("INFO: Schema version 1 -> 9\n{\n  \"status\": \"ok\"\n}\n")
	if got := strings.TrimSpace(string(lastJSONObject(raw))); got != "{\n  \"status\": \"ok\"\n}" {
		t.Fatalf("pretty object not recovered: %q", got)
	}
	// Single-line object.
	one := []byte(`{"nodes": 1}`)
	if got := string(lastJSONObject(one)); got != `{"nodes": 1}` {
		t.Fatalf("single-line object not recovered: %q", got)
	}
}

// TestLastJSONObject_NestedResultObjects is the regression for the shape a
// live `code-review-graph query` returns whenever it finds anything: a
// pretty-printed document whose results array holds indented objects. Matching
// the last INDENTED brace truncates the document mid-array and decoding fails
// with "invalid character ']' after top-level value" — which reads as a graph
// failure and blocks a plan that should have succeeded.
func TestLastJSONObject_NestedResultObjects(t *testing.T) {
	t.Parallel()
	raw := []byte(`INFO: noise on the same stream
{
  "status": "ok",
  "result_count": 2,
  "results": [
    {
      "kind": "Test",
      "name": "TestOne",
      "file_path": "/repo/pkg/a/a_test.go"
    },
    {
      "kind": "Test",
      "name": "TestTwo",
      "file_path": "/repo/pkg/b/b_test.go"
    }
  ]
}
`)
	var res graphQueryResult
	if err := json.Unmarshal(lastJSONObject(raw), &res); err != nil {
		t.Fatalf("nested result objects must decode: %v", err)
	}
	if res.Status != "ok" || res.ResultCount != 2 || len(res.Results) != 2 {
		t.Fatalf("decoded %+v", res)
	}
	if res.Results[1].Name != "TestTwo" {
		t.Fatalf("last result lost: %+v", res.Results)
	}
}

func TestRelativeToRoot_DropsOutsideAndAbsolute(t *testing.T) {
	t.Parallel()
	root := "/repo"
	if got := relativeToRoot("/repo/pkg/a/a.go", root); got != "pkg/a/a.go" {
		t.Fatalf("got %q", got)
	}
	if got := relativeToRoot("/elsewhere/x.go", root); got != "" {
		t.Fatalf("paths outside the worktree must be dropped, got %q", got)
	}
	if got := relativeToRoot("/repo", root); got != "" {
		t.Fatalf("the root itself is not a file, got %q", got)
	}
	if got := relativeToRoot("pkg/b/b.go", root); got != "pkg/b/b.go" {
		t.Fatalf("got %q", got)
	}
}

func TestDisplayTargetStripsRoot(t *testing.T) {
	t.Parallel()
	got := DisplayTarget("tests_for(/w/repo/pkg/a/a.go::X) at /w/repo/pkg/a", "/w/repo")
	if strings.Contains(got, "/w/repo") {
		t.Fatalf("root not stripped: %q", got)
	}
	if DisplayTarget("unchanged", "") != "unchanged" {
		t.Fatal("empty root must be a no-op")
	}
}
