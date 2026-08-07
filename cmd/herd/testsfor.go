package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/graph"
)

// FAC-160: `herd tests-for <base>..<candidate>` is the compiled production
// caller of pkg/graph.Plan. It proves the local code-review-graph index is
// bound to the exact candidate revision AND covers the tracked-source
// manifest before any not_found / zero-tests answer is allowed to narrow
// verification. A graph that reports the right commit while silently omitting
// tracked files cannot return a trusted absence (live audit 2026-08-03: 196
// of 302 files indexed, functions reported not_found at a SHA where they
// existed).
//
// Exit 0 only when the plan rests on proven-complete evidence. Exit 1 with a
// BLOCKED report and a broadened (full-profile) plan otherwise.

// maxGraphQueries bounds the query fan-out for very large change sets. When it
// trips the report says so explicitly and the plan escalates — a truncated
// query set must never read as "the graph found nothing".
const maxGraphQueries = 200

// testsForReport is the durable structured evidence emitted per run.
type testsForReport struct {
	Command        string               `json:"command"`
	BaseSHA        string               `json:"base_sha"`
	CandidateSHA   string               `json:"candidate_sha"`
	ChangedPaths   []string             `json:"changed_paths"`
	ChangedSymbols []string             `json:"changed_symbols,omitempty"`
	RejectedPaths  []string             `json:"rejected_paths,omitempty"`
	Integrity      graph.GraphIntegrity `json:"graph_integrity"`
	Blocked        bool                 `json:"blocked"`
	BlockedReasons []string             `json:"blocked_reasons,omitempty"`
	Plan           *graph.TestPlan      `json:"plan"`
}

func runTestsFor() {
	fs := flag.NewFlagSet("tests-for", flag.ExitOnError)
	tool := fs.String("graph-tool", graph.DefaultGraphTool, "code-review-graph executable (exact argv, never shelled)")
	noRebuild := fs.Bool("no-rebuild", false, "Do not attempt the single bounded full rebuild on incomplete parity")
	timeout := fs.Duration("timeout", 20*time.Minute, "Overall deadline for graph tool invocations")
	_ = fs.Parse(os.Args[2:])

	base, cand, err := parseRevRange(fs.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: %v\n", err)
		fmt.Fprintln(os.Stderr, "usage: herd tests-for [flags] <base>..<candidate>")
		fmt.Fprintln(os.Stderr, "       flags precede the revision range; use -- for a literal ref")
		os.Exit(2)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	root, err := gitText(ctx, ".", "rev-parse", "--show-toplevel")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: repo root: %v\n", err)
		os.Exit(1)
	}
	root = strings.TrimSpace(root)

	baseSHA, err := resolveCommit(ctx, root, base)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: base %q: %v\n", base, err)
		os.Exit(1)
	}
	candSHA, err := resolveCommit(ctx, root, cand)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: candidate %q: %v\n", cand, err)
		os.Exit(1)
	}

	rawChanged, err := gitZList(ctx, root, "diff", "--name-only", "-z", baseSHA, candSHA, "--")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: diff: %v\n", err)
		os.Exit(1)
	}
	rawTracked, err := gitZList(ctx, root, "ls-tree", "-r", "--name-only", "-z", candSHA, "--")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: ls-tree: %v\n", err)
		os.Exit(1)
	}

	// Any path git reports that does not survive repo-relative normalization
	// would escape the worktree. Record it and refuse to narrow.
	changed, rejected := splitEscapingPaths(rawChanged)
	manifest := graph.BuildManifest(rawTracked, graph.DefaultSourceExts)

	diffText, err := gitText(ctx, root, "diff", "-U0", baseSHA, candSHA, "--")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: diff -U0: %v\n", err)
		os.Exit(1)
	}
	symbols, symbolFiles := changedExportedGoSymbols(diffText)

	targets, truncated := graphTargets(root, changed, symbolFiles)

	ev, integ, cerr := graph.CollectEvidence(ctx, execRunner(root), graph.EvidenceRequest{
		Tool:         *tool,
		RepoRoot:     root,
		Commit:       candSHA,
		Manifest:     manifest,
		Targets:      targets,
		AllowRebuild: !*noRebuild,
	})
	if cerr != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: graph evidence: %v\n", cerr)
		os.Exit(1)
	}

	blockedReasons := append([]string(nil), integ.RejectionReasons...)
	for _, p := range rejected {
		blockedReasons = append(blockedReasons, "path escapes worktree: "+p)
	}
	if truncated {
		blockedReasons = append(blockedReasons,
			fmt.Sprintf("query set truncated at %d targets; absence is not authoritative", maxGraphQueries))
	}
	sort.Strings(blockedReasons)
	blocked := len(blockedReasons) > 0

	in := graph.PlanInput{
		BaseSHA:        baseSHA,
		CandidateSHA:   candSHA,
		ChangedPaths:   changed,
		ChangedSymbols: symbols,
		Graph:          ev,
		Profile:        graph.DefaultGoProfile(),
		// The index must be built on the candidate tree: edges about symbols the
		// candidate introduces cannot exist in an index built at the base.
		GraphAnchorSHA: candSHA,
	}
	if blocked {
		// Broaden rather than emit an empty targeted plan.
		in.Graph = graph.GraphEvidence{}
		in.ForceEscalate = "graph integrity unproven: " + strings.Join(blockedReasons, "; ")
	}

	plan, perr := graph.Plan(in)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: plan: %v\n", perr)
		os.Exit(1)
	}

	rep := testsForReport{
		Command:        "tests-for",
		BaseSHA:        baseSHA,
		CandidateSHA:   candSHA,
		ChangedPaths:   changed,
		ChangedSymbols: symbols,
		RejectedPaths:  rejected,
		Integrity:      integ,
		Blocked:        blocked,
		BlockedReasons: blockedReasons,
		Plan:           plan,
	}
	out, merr := json.MarshalIndent(rep, "", "  ")
	if merr != nil {
		fmt.Fprintf(os.Stderr, "herd tests-for: marshal: %v\n", merr)
		os.Exit(1)
	}
	fmt.Println(string(out))
	if blocked {
		fmt.Fprintf(os.Stderr, "BLOCKED tests-for %s..%s: %s\n", baseSHA, candSHA, strings.Join(blockedReasons, "; "))
		os.Exit(1)
	}
}

// parseRevRange accepts `<base>..<candidate>` or two positional refs.
func parseRevRange(args []string) (string, string, error) {
	var base, cand string
	switch len(args) {
	case 1:
		parts := strings.SplitN(args[0], "..", 2)
		if len(parts) != 2 {
			return "", "", fmt.Errorf("expected <base>..<candidate>, got %q", args[0])
		}
		base, cand = strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	case 2:
		base, cand = strings.TrimSpace(args[0]), strings.TrimSpace(args[1])
	default:
		return "", "", fmt.Errorf("expected exactly one <base>..<candidate> argument")
	}
	if err := refSafety(base); err != nil {
		return "", "", fmt.Errorf("base: %w", err)
	}
	if err := refSafety(cand); err != nil {
		return "", "", fmt.Errorf("candidate: %w", err)
	}
	return base, cand, nil
}

// refSafety rejects refs that could be read as git options or that carry
// control bytes. The ref is still passed as one exact argv element after
// --end-of-options; this is defense in depth, not the only barrier.
func refSafety(r string) error {
	r = strings.TrimSpace(r)
	if r == "" {
		return fmt.Errorf("empty ref")
	}
	if strings.HasPrefix(r, "-") {
		return fmt.Errorf("ref %q may not start with '-'", r)
	}
	if strings.ContainsAny(r, "\x00\n\r") {
		return fmt.Errorf("ref contains control characters")
	}
	return nil
}

// resolveCommit pins a ref to an exact commit SHA. --end-of-options guarantees
// the ref is data even if it looks like a flag.
func resolveCommit(ctx context.Context, root, ref string) (string, error) {
	out, err := gitText(ctx, root, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(out)
	if len(sha) != 40 {
		return "", fmt.Errorf("unexpected rev-parse output %q", sha)
	}
	return sha, nil
}

func gitText(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// gitZList runs a NUL-delimited git listing. NUL separation keeps paths with
// spaces or newlines exact.
func gitZList(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := gitText(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	var list []string
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			list = append(list, p)
		}
	}
	return list, nil
}

// splitEscapingPaths returns repo-relative paths and the raw entries that did
// not survive normalization (i.e. would escape the worktree root).
func splitEscapingPaths(raw []string) (keep []string, rejected []string) {
	for _, p := range raw {
		clean := path.Clean(strings.ReplaceAll(strings.TrimSpace(p), "\\", "/"))
		if clean == "" || clean == "." || clean == ".." ||
			strings.HasPrefix(clean, "/") || strings.HasPrefix(clean, "../") {
			rejected = append(rejected, p)
			continue
		}
		keep = append(keep, clean)
	}
	sort.Strings(keep)
	sort.Strings(rejected)
	return keep, rejected
}

// exportedGoDecl matches an added Go declaration with an exported name.
var exportedGoDecl = regexp.MustCompile(
	`^\+(?:func\s+(?:\([^)]*\)\s*)?([A-Z]\w*)|(?:type|var|const)\s+([A-Z]\w*))\b`)

// diffFileHeader matches the post-image path line of a unified diff.
var diffFileHeader = regexp.MustCompile(`^\+\+\+ b/(.+)$`)

// changedExportedGoSymbols extracts exported Go declarations added or modified
// by the diff, plus the repo-relative file each was found in.
func changedExportedGoSymbols(diff string) ([]string, map[string][]string) {
	byFile := map[string][]string{}
	seen := map[string]struct{}{}
	var symbols []string
	current := ""
	for _, line := range strings.Split(diff, "\n") {
		if m := diffFileHeader.FindStringSubmatch(line); m != nil {
			current = strings.TrimSpace(m[1])
			continue
		}
		if current == "" || !strings.HasSuffix(current, ".go") || strings.HasSuffix(current, "_test.go") {
			continue
		}
		m := exportedGoDecl.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if name == "" {
			continue
		}
		key := current + "::" + name
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		byFile[current] = append(byFile[current], name)
		symbols = append(symbols, name)
	}
	sort.Strings(symbols)
	for f := range byFile {
		sort.Strings(byFile[f])
	}
	return dedupeSorted(symbols), byFile
}

func dedupeSorted(in []string) []string {
	var out []string
	for i, s := range in {
		if i == 0 || s != in[i-1] {
			out = append(out, s)
		}
	}
	return out
}

// graphTargets builds the deterministic query set: tests_for/callers_of per
// changed exported symbol and importers_of per changed source file. Qualified
// names are absolute because that is the identity code-review-graph stores;
// they are argv only and never reach the receipt (hits are relativized).
func graphTargets(root string, changed []string, symbolFiles map[string][]string) ([]graph.QueryTarget, bool) {
	var targets []graph.QueryTarget
	files := make([]string, 0, len(symbolFiles))
	for f := range symbolFiles {
		files = append(files, f)
	}
	sort.Strings(files)
	for _, f := range files {
		for _, sym := range symbolFiles[f] {
			qn := root + "/" + f + "::" + sym
			targets = append(targets,
				graph.QueryTarget{Pattern: "tests_for", Target: qn},
				graph.QueryTarget{Pattern: "callers_of", Target: qn})
		}
	}
	for _, f := range changed {
		if strings.HasSuffix(f, ".go") {
			targets = append(targets, graph.QueryTarget{Pattern: "importers_of", Target: root + "/" + f})
		}
	}
	if len(targets) > maxGraphQueries {
		return targets[:maxGraphQueries], true
	}
	return targets, false
}

// execRunner runs an exact argv with no shell. Repository paths contain spaces
// and refs are attacker-influenced data; neither is ever tokenized.
func execRunner(dir string) graph.Runner {
	return func(ctx context.Context, argv []string) ([]byte, error) {
		if len(argv) == 0 {
			return nil, fmt.Errorf("empty argv")
		}
		cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
		cmd.Dir = dir
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		if err != nil {
			return out, fmt.Errorf("%s: %w: %s", argv[0], err, strings.TrimSpace(stderr.String()))
		}
		return out, nil
	}
}
