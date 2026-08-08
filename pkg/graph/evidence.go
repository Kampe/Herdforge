package graph

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
)

// DefaultGraphTool is the code-review-graph executable. Callers may override
// it (herd tests-for --graph-tool) so hermetic tests run without the real CLI.
const DefaultGraphTool = "code-review-graph"

// DefaultSourceExts is the tracked-source extension set the local graph is
// expected to index for a Go repository profile. The manifest is deliberately
// explicit: deriving it from whatever the graph happens to contain would make
// the completeness check circular (a graph missing every .py file would report
// no python and pass).
var DefaultSourceExts = []string{".go", ".sql"}

// SourceManifest is the tracked-source file set the index must cover at an
// exact commit. Files are repo-relative, sorted and unique.
type SourceManifest struct {
	Files  []string `json:"-"`
	Digest string   `json:"manifest_digest"`
}

// BuildManifest filters tracked repo-relative paths to the supported source
// extensions and binds them to a stable digest. Paths that escape the repo
// root are dropped by normalizeRepoPath and never enter the manifest.
func BuildManifest(tracked []string, exts []string) SourceManifest {
	want := make(map[string]struct{}, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimSpace(e))
		if e != "" {
			want[e] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(tracked))
	var files []string
	for _, p := range tracked {
		rel := normalizeRepoPath(p)
		if rel == "" {
			continue
		}
		if _, ok := want[strings.ToLower(path.Ext(rel))]; !ok {
			continue
		}
		if _, dup := seen[rel]; dup {
			continue
		}
		seen[rel] = struct{}{}
		files = append(files, rel)
	}
	sort.Strings(files)
	sum := sha256.Sum256([]byte(strings.Join(files, "\n")))
	return SourceManifest{Files: files, Digest: hex.EncodeToString(sum[:])}
}

// GraphIntegrity is the durable structured evidence that binds a graph-derived
// plan to the tool, the exact repository commit, the tracked-source manifest,
// the graph counts and the executed query set. Trusted is the only field a
// consumer may read as "absence is authoritative".
type GraphIntegrity struct {
	Tool            string   `json:"tool"`
	RepoCommit      string   `json:"repo_commit"`
	GraphBuiltAt    string   `json:"graph_built_at,omitempty"`
	GraphCurrentSHA string   `json:"graph_current_sha,omitempty"`
	ManifestFiles   int      `json:"manifest_files"`
	ManifestDigest  string   `json:"manifest_digest"`
	GraphFiles      int      `json:"graph_files"`
	GraphNodes      int      `json:"graph_nodes"`
	GraphEdges      int      `json:"graph_edges"`
	Queries         []string `json:"queries,omitempty"`
	// UnresolvedTargets are query targets the index has no node for at all
	// (status not_found) — typically a package-level const or var, which the
	// graph does not model as a node. Against a proven-complete index that is
	// authoritative for "this symbol has no graph identity", which means its
	// coverage cannot be derived. It is NOT a zero-result query and must never
	// be counted as one; callers broaden verification instead.
	UnresolvedTargets []string `json:"unresolved_targets,omitempty"`
	Rebuilt           bool     `json:"rebuilt"`
	Trusted           bool     `json:"trusted"`
	RejectionReasons  []string `json:"rejection_reasons,omitempty"`
}

// CheckIntegrity returns sorted rejection reasons for graph evidence at commit.
// An empty result means the index is bound to that exact revision AND covers
// the whole tracked-source manifest, which is the only state in which a
// not_found / zero-callers / zero-tests answer may be read as authoritative.
//
// The manifest-parity clause is the load-bearing one: on 2026-08-03 the graph
// reported the correct commit while holding 196 of 302 files and answered
// not_found for functions that existed at that SHA.
func CheckIntegrity(status GraphStatusReport, commit string, m SourceManifest) []string {
	var reasons []string
	commit = strings.TrimSpace(commit)
	if commit == "" {
		reasons = append(reasons, "candidate commit is empty; evidence cannot be revision-bound")
	}
	switch {
	case status.BuiltAtCommit == "":
		reasons = append(reasons, "graph reports no built_at_commit (unbuilt index)")
	case commit != "" && status.BuiltAtCommit != commit:
		reasons = append(reasons, fmt.Sprintf("graph built_at_commit=%s != candidate=%s", status.BuiltAtCommit, commit))
	}
	if commit != "" && status.CurrentSHA != "" && status.CurrentSHA != commit {
		reasons = append(reasons, fmt.Sprintf("graph current_sha=%s != candidate=%s (working tree moved)", status.CurrentSHA, commit))
	}
	if len(m.Files) == 0 {
		reasons = append(reasons, "tracked-source manifest is empty; nothing to prove coverage against")
	} else if status.Files < len(m.Files) {
		reasons = append(reasons, fmt.Sprintf(
			"index incomplete: graph files=%d < tracked source manifest=%d (manifest_digest=%s)",
			status.Files, len(m.Files), m.Digest))
	}
	if status.Nodes <= 0 && len(m.Files) > 0 {
		reasons = append(reasons, "graph holds zero nodes; absence proves nothing")
	}
	return uniqueSortedStrings(reasons)
}

// Runner executes one exact argv and returns its stdout. Implementations must
// never pass argv through a shell: repository paths contain spaces and refs are
// attacker-influenced data.
type Runner func(ctx context.Context, argv []string) ([]byte, error)

// QueryTarget is one graph query to execute. Pattern is a code-review-graph
// query pattern (tests_for, callers_of, importers_of); Target is a qualified
// name or file path passed as a single argv element.
type QueryTarget struct {
	Pattern string
	Target  string
}

// EvidenceRequest is the adapter input.
type EvidenceRequest struct {
	// Tool is the code-review-graph executable (DefaultGraphTool when empty).
	Tool string
	// RepoRoot is the absolute worktree root handed to --repo and stripped from
	// returned file paths so no absolute host path reaches a receipt.
	RepoRoot string
	// Commit is the exact candidate SHA the evidence must be bound to.
	Commit string
	// Manifest is the tracked-source manifest at Commit.
	Manifest SourceManifest
	// Targets are the queries to run once the index is proven complete.
	Targets []QueryTarget
	// AllowRebuild permits exactly one bounded full rebuild when the first
	// integrity check fails.
	AllowRebuild bool
}

// CollectEvidence proves index completeness at an exact revision and, only
// then, runs the query set. A failed check is never downgraded to "no hits":
// Trusted stays false and Hits stay empty so the caller must broaden
// verification instead of emitting an empty targeted plan.
func CollectEvidence(ctx context.Context, run Runner, req EvidenceRequest) (GraphEvidence, GraphIntegrity, error) {
	if run == nil {
		return GraphEvidence{}, GraphIntegrity{}, errors.New("graph runner is required")
	}
	tool := strings.TrimSpace(req.Tool)
	if tool == "" {
		tool = DefaultGraphTool
	}
	integ := GraphIntegrity{
		Tool:           tool,
		RepoCommit:     strings.TrimSpace(req.Commit),
		ManifestFiles:  len(req.Manifest.Files),
		ManifestDigest: req.Manifest.Digest,
	}

	status, err := graphStatus(ctx, run, tool, req.RepoRoot)
	if err != nil {
		integ.RejectionReasons = []string{DisplayTarget("graph status failed: "+err.Error(), req.RepoRoot)}
		return GraphEvidence{}, integ, nil
	}
	reasons := CheckIntegrity(status, req.Commit, req.Manifest)

	// One bounded full rebuild, then re-check parity. Never loop.
	if len(reasons) > 0 && req.AllowRebuild {
		if _, berr := run(ctx, append([]string{tool, "build"}, repoFlag(req.RepoRoot)...)); berr != nil {
			reasons = append(reasons, DisplayTarget("full rebuild failed: "+berr.Error(), req.RepoRoot))
		} else {
			integ.Rebuilt = true
			status, err = graphStatus(ctx, run, tool, req.RepoRoot)
			if err != nil {
				reasons = append(reasons, DisplayTarget("graph status after rebuild failed: "+err.Error(), req.RepoRoot))
			} else {
				reasons = CheckIntegrity(status, req.Commit, req.Manifest)
			}
		}
		reasons = uniqueSortedStrings(reasons)
	}

	integ.GraphBuiltAt = status.BuiltAtCommit
	integ.GraphCurrentSHA = status.CurrentSHA
	integ.GraphFiles = status.Files
	integ.GraphNodes = status.Nodes
	integ.GraphEdges = status.Edges

	if len(reasons) > 0 {
		integ.RejectionReasons = reasons
		return GraphEvidence{}, integ, nil
	}

	hits, queries, unresolved, qerr := runQueries(ctx, run, tool, req.RepoRoot, req.Targets)
	integ.Queries = queries
	integ.UnresolvedTargets = unresolved
	if qerr != nil {
		integ.RejectionReasons = []string{DisplayTarget("graph query failed: "+qerr.Error(), req.RepoRoot)}
		return GraphEvidence{}, integ, nil
	}
	integ.Trusted = true
	return EvidenceFromStatus(status, hits), integ, nil
}

func repoFlag(root string) []string {
	if strings.TrimSpace(root) == "" {
		return nil
	}
	return []string{"--repo", root}
}

func graphStatus(ctx context.Context, run Runner, tool, root string) (GraphStatusReport, error) {
	argv := append([]string{tool, "status", "--json"}, repoFlag(root)...)
	out, err := run(ctx, argv)
	if err != nil {
		return GraphStatusReport{}, err
	}
	return ParseGraphStatusJSON(lastJSONObject(out))
}

// lastJSONObject returns the buffer from the last line that opens a TOP-LEVEL
// JSON object through the end. The tool prefixes schema-migration notices on
// some runs, so "parse the whole buffer" fails; it also pretty-prints objects,
// so "parse the last line" fails. The open brace must be at column zero:
// nested result objects are indented, and matching one of those truncates the
// document mid-array ("invalid character ']' after top-level value").
func lastJSONObject(raw []byte) []byte {
	lines := strings.Split(string(raw), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(lines[i], "{") {
			return []byte(strings.Join(lines[i:], "\n"))
		}
	}
	return raw
}

// graphQueryResult is the subset of `code-review-graph query` output used here.
type graphQueryResult struct {
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	ResultCount int    `json:"result_count"`
	Results     []struct {
		Kind          string `json:"kind"`
		Name          string `json:"name"`
		QualifiedName string `json:"qualified_name"`
		FilePath      string `json:"file_path"`
		IsTest        bool   `json:"is_test"`
	} `json:"results"`
}

// runQueries executes the query set and returns hits, the deterministic record
// of what was asked, and the targets the index holds no node for.
//
// `code-review-graph query` exits 0 for ok, ambiguous and not_found alike, so
// status is checked explicitly and each case is handled differently:
//
//   - ok: results become hits and the query count is recorded.
//   - not_found: the target has no node (a package-level const or var is not
//     modelled as one). Recorded as unresolved so the caller broadens; it is
//     never recorded as a zero-result query, which would read as proven
//     coverage.
//   - anything else (ambiguous, error): a hard failure. An ambiguous target
//     means the question was meaningless, and real edges may hide under
//     another node.
func runQueries(ctx context.Context, run Runner, tool, root string, targets []QueryTarget) ([]GraphHit, []string, []string, error) {
	var hits []GraphHit
	var queries []string
	var unresolved []string
	for _, t := range targets {
		pattern := strings.TrimSpace(t.Pattern)
		target := strings.TrimSpace(t.Target)
		if pattern == "" || target == "" {
			continue
		}
		// The graph stores absolute qualified names, so the query argument is
		// absolute; every recorded form is relativized first. Absolute host
		// paths must never reach a receipt or a rejection reason.
		shown := DisplayTarget(target, root)
		argv := append([]string{tool, "query", pattern, target}, repoFlag(root)...)
		out, err := run(ctx, argv)
		if err != nil {
			return nil, queries, unresolved, fmt.Errorf("%s(%s): %w", pattern, shown, err)
		}
		var res graphQueryResult
		if jerr := json.Unmarshal(lastJSONObject(out), &res); jerr != nil {
			return nil, queries, unresolved, fmt.Errorf("%s(%s): decode: %w", pattern, shown, jerr)
		}
		if res.Status == "not_found" {
			unresolved = append(unresolved, shown)
			continue
		}
		if res.Status != "ok" {
			return nil, queries, unresolved, fmt.Errorf("%s(%s): status=%s %s", pattern, shown, res.Status, DisplayTarget(res.Summary, root))
		}
		queries = append(queries, pattern+"("+shown+")="+fmt.Sprint(res.ResultCount))
		for _, r := range res.Results {
			rel := relativeToRoot(r.FilePath, root)
			if rel == "" {
				continue
			}
			hits = append(hits, GraphHit{
				Kind:     pattern,
				Target:   target,
				FilePath: rel,
				Symbol:   r.Name,
			})
		}
	}
	sort.Strings(queries)
	return hits, queries, uniqueSortedStrings(unresolved), nil
}

// DisplayTarget strips the worktree root from any text so evidence, receipts
// and rejection reasons stay repo-relative. It is a pure string rewrite: it
// never resolves or validates a path.
func DisplayTarget(s, root string) string {
	root = strings.TrimSuffix(strings.TrimSpace(root), "/")
	if root == "" {
		return s
	}
	return strings.ReplaceAll(s, root+"/", "")
}

// relativeToRoot converts an absolute graph file path to a repo-relative one.
// Absolute host paths must never reach a receipt; anything outside the root is
// dropped rather than recorded.
func relativeToRoot(p, root string) string {
	p = strings.TrimSpace(p)
	root = strings.TrimSpace(root)
	if p == "" {
		return ""
	}
	if root != "" {
		trimmed := strings.TrimSuffix(root, "/")
		if p == trimmed {
			return ""
		}
		if strings.HasPrefix(p, trimmed+"/") {
			p = strings.TrimPrefix(p, trimmed+"/")
		} else if strings.HasPrefix(p, "/") {
			return ""
		}
	}
	return normalizeRepoPath(p)
}
