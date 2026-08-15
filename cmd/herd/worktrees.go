package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/remoteci"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

const worktreesSchemaVersion = 2

// Snapshot is the versioned machine-readable contract emitted by
// `herd worktrees --json`.
type Snapshot struct {
	SchemaVersion int   `json:"schema_version"`
	Worktrees     []Row `json:"worktrees"`
}

// Row is a single worktree snapshot. Fields match the JSON schema from
// bin/herd-worktrees and the human column-t table.
type Row struct {
	Worktree     string        `json:"worktree"`
	Branch       string        `json:"branch"`
	Head         string        `json:"head"`
	Locked       bool          `json:"locked"`
	Ahead        int           `json:"ahead"`
	Dirty        int           `json:"dirty"`
	Files        []string      `json:"files"`
	CandidateSHA string        `json:"candidate_sha"`
	Fleet        FleetSnapshot `json:"fleet"`
}

// FleetSnapshot contains only authoritative values this command directly
// reads. "unavailable" never means a pass or an absent owner.
type FleetSnapshot struct {
	Lease        LeaseSnapshot       `json:"lease"`
	Session      EvidenceState       `json:"session"`
	SafeRef      SafeRefSnapshot     `json:"safe_ref"`
	Retention    EvidenceState       `json:"retention"`
	CI           EvidenceState       `json:"ci"`
	Verification []VerificationState `json:"verification"`
}

type EvidenceState struct {
	State string `json:"state"`
}

type LeaseSnapshot struct {
	State      string `json:"state"`
	Lane       string `json:"lane,omitempty"`
	Owner      string `json:"owner,omitempty"`
	Role       string `json:"role,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

type SafeRefSnapshot struct {
	State string `json:"state"`
	Ref   string `json:"ref,omitempty"`
	SHA   string `json:"sha,omitempty"`
}

type VerificationState struct {
	State          string `json:"state"`
	CandidateSHA   string `json:"candidate_sha"`
	PolicyRevision string `json:"policy_revision"`
	Attempt        int64  `json:"attempt"`
}

// collision maps a file path to the set of worktree branches that touch it.
type collision struct {
	File     string
	Branches []string
}

func runWorktrees() {
	fs := flag.NewFlagSet("worktrees", flag.ContinueOnError)
	jsonFlag := fs.Bool("json", false, "Output JSON array")
	filesFlag := fs.Bool("files", false, "Also list per-worktree touched files")
	usage := func() {
		fmt.Println(`herd-worktrees , one-shot collision snapshot across every repo worktree:
  {worktree, branch, ahead-of-origin/main commits, dirty files, touched files}.
  Replaces the coordinator's manual per-worktree ` + "`git log` + `git status`" + `
  loop when ranking claimable tickets (planner request 2026-07-21). A final
  COLLISIONS section lists any file touched (committed-ahead or dirty) by
  more than one worktree, so collision-checking is exact, not eyeballed.

    herd worktrees            # human summary + collisions
    herd worktrees --json     # machine-readable array
    herd worktrees --files    # also list per-worktree touched files`)
	}
	fs.Usage = usage

	// Handle --help/-h/-help before flag parse (single-dash -help is a
	// valid Go flag synonym for --help but the flag package returns
	// ErrHelp which our error path would exit-code 2; catch it here).
	for _, a := range os.Args[2:] {
		if a == "--help" || a == "-h" || a == "-help" {
			usage()
			os.Exit(0)
		}
	}

	// Suppress the flag package's own error/usage output so unknown-flag
	// messages match the zsh contract: stderr "unknown arg X", no usage.
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}

	if err := fs.Parse(os.Args[2:]); err != nil {
		if err == flag.ErrHelp {
			usage()
			os.Exit(0)
		}
		// Identify the first unknown arg from the original argv, not
		// the flag package's normalised error text.
		for _, a := range os.Args[2:] {
			if a == "--json" || a == "--files" || a == "--help" || a == "-h" || a == "-help" {
				continue
			}
			fmt.Fprintf(os.Stderr, "herd-worktrees: unknown arg %s\n", a)
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "herd-worktrees: %v\n", err)
		os.Exit(2)
	}

	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "herd-worktrees: unknown arg %s\n", fs.Arg(0))
		os.Exit(2)
	}

	// Resolve repo root
	repoRoot := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	absRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		absRoot = repoRoot
	}

	// Verify origin/main exists
	ctx := context.Background()
	if err := runGit(ctx, absRoot, "rev-parse", "-q", "--verify", "origin/main"); err != nil {
		fmt.Fprintln(os.Stderr, "herd-worktrees: origin/main not found")
		os.Exit(1)
	}

	base := "origin/main"

	// Enumerate worktrees from principal repo
	out, err := gitOutput(ctx, absRoot, "worktree", "list", "--porcelain")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd-worktrees: git worktree list: %v\n", err)
		os.Exit(1)
	}

	type wtEntry struct {
		Path   string
		Locked bool
	}

	var entries []wtEntry
	blocks := strings.Split(strings.TrimRight(string(out), "\n"), "\n\n")
	for _, block := range blocks {
		lines := strings.Split(block, "\n")
		if len(lines) == 0 {
			continue
		}
		if !strings.HasPrefix(lines[0], "worktree ") {
			continue
		}
		path := strings.TrimPrefix(lines[0], "worktree ")
		locked := false
		for _, line := range lines[1:] {
			if line == "locked" {
				locked = true
				break
			}
		}
		entries = append(entries, wtEntry{Path: path, Locked: locked})
	}

	// Pre-allocate so JSON marshals to [] not null when no worktrees
	// survive the missing-dir skip.
	rows := make([]Row, 0, len(entries))

	for _, e := range entries {
		// Skip vanished worktree directories
		if _, err := os.Stat(e.Path); os.IsNotExist(err) {
			continue
		}

		r := Row{
			Worktree: e.Path,
			Locked:   e.Locked,
			Files:    []string{},
		}

		r.Branch = runGitOrDefault(ctx, e.Path, "?", "rev-parse", "--abbrev-ref", "HEAD")
		r.Head = runGitOrDefault(ctx, e.Path, "?", "rev-parse", "--short", "HEAD")
		r.CandidateSHA = runGitOrDefault(ctx, e.Path, "", "rev-parse", "HEAD")

		// Patch-equivalence count via git cherry
		r.Ahead = countCherryAhead(ctx, e.Path, base)

		// Committed (ahead) files
		var committedFiles []string
		if r.Ahead > 0 {
			cfOut, err := gitOutput(ctx, e.Path, "diff", "--name-only", base+"...HEAD")
			if err == nil {
				raw := strings.TrimSpace(string(cfOut))
				if raw != "" {
					committedFiles = strings.Split(raw, "\n")
				}
			}
		}

		// Dirty files
		dirtyOut, err := gitOutput(ctx, e.Path, "status", "--porcelain")
		if err == nil {
			raw := strings.TrimSpace(string(dirtyOut))
			if raw != "" {
				lines := strings.Split(raw, "\n")
				for _, line := range lines {
					// Strip the XY status column, keep the path
					if len(line) > 3 {
						pathPart := strings.TrimSpace(line[2:])
						if pathPart != "" {
							r.Files = append(r.Files, pathPart)
						}
					}
				}
				r.Dirty = len(lines)
			}
		}

		// Union committed + dirty files
		r.Files = append(r.Files, committedFiles...)
		if len(r.Files) > 0 {
			sort.Strings(r.Files)
			// Deduplicate
			seen := make(map[string]bool)
			uniq := make([]string, 0, len(r.Files))
			for _, f := range r.Files {
				if !seen[f] {
					seen[f] = true
					uniq = append(uniq, f)
				}
			}
			r.Files = uniq
		}

		rows = append(rows, r)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Branch != rows[j].Branch {
			return rows[i].Branch < rows[j].Branch
		}
		return rows[i].Worktree < rows[j].Worktree
	})
	for i := range rows {
		rows[i].Fleet = loadFleetSnapshot(ctx, absRoot, rows[i])
	}

	// Early exit for --json
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(Snapshot{SchemaVersion: worktreesSchemaVersion, Worktrees: rows})
		os.Exit(0)
	}

	// Human output
	printHuman(rows, *filesFlag)

	// Collisions
	cols := computeCollisions(rows)
	if len(cols) > 0 {
		fmt.Println("COLLISIONS:")
		for _, c := range cols {
			fmt.Printf("  %s  <-  %s\n", c.File, strings.Join(c.Branches, ", "))
		}
		os.Exit(3)
	}
	fmt.Println("COLLISIONS: none")
}

func unavailableFleetSnapshot() FleetSnapshot {
	return FleetSnapshot{
		Lease:        LeaseSnapshot{State: "unavailable"},
		Session:      EvidenceState{State: "unavailable"},
		SafeRef:      SafeRefSnapshot{State: "unavailable"},
		Retention:    EvidenceState{State: "unavailable"},
		CI:           EvidenceState{State: "unavailable"},
		Verification: []VerificationState{},
	}
}

func loadFleetSnapshot(ctx context.Context, root string, row Row) FleetSnapshot {
	snapshot := unavailableFleetSnapshot()
	loadLeaseSnapshot(ctx, root, row.Worktree, &snapshot)
	loadSafeRefSnapshot(ctx, root, row.Branch, &snapshot)
	loadVerificationSnapshot(root, row.CandidateSHA, &snapshot)
	return snapshot
}

func loadLeaseSnapshot(ctx context.Context, root, path string, snapshot *FleetSnapshot) {
	dbPath := filepath.Join(root, ".herd", "herdforge.db")
	if _, err := os.Stat(dbPath); err != nil {
		return
	}
	store, err := claim.NewSQLiteLeaseStore(dbPath)
	if err != nil {
		return
	}
	defer store.Close()
	leases, err := store.ActiveClaims(ctx, time.Now())
	if err != nil {
		return
	}
	for _, lease := range leases {
		if lease == nil || !sameWorktreePath(lease.WorktreePath, path) {
			continue
		}
		snapshot.Lease = LeaseSnapshot{State: "available", Lane: lease.HoldLane, Owner: lease.OwnerID, Role: lease.Role, Generation: lease.Generation}
		return
	}
}

func loadSafeRefSnapshot(ctx context.Context, root, branch string, snapshot *FleetSnapshot) {
	ref, ok := taskSafeRef(branch)
	if !ok {
		return
	}
	sha := runGitOrDefault(ctx, root, "", "rev-parse", "--verify", ref)
	if sha != "" {
		snapshot.SafeRef = SafeRefSnapshot{State: "available", Ref: ref, SHA: sha}
	}
}

func taskSafeRef(branch string) (string, bool) {
	const prefix = "herd/"
	if !strings.HasPrefix(strings.ToLower(branch), prefix) {
		return "", false
	}
	ref := strings.TrimSpace(branch[len(prefix):])
	if ref == "" {
		return "", false
	}
	return worktree.SafeRefFor(ref), true
}

func loadVerificationSnapshot(root, candidateSHA string, snapshot *FleetSnapshot) {
	if len(candidateSHA) != 40 {
		return
	}
	ledgerPath := filepath.Join(root, ".herd", "remote-ci.jsonl")
	if _, err := os.Stat(ledgerPath); err != nil {
		return
	}
	store, err := remoteci.Open(ledgerPath)
	if err != nil {
		return
	}
	settlements, err := store.List()
	if err != nil {
		return
	}
	snapshot.CI = EvidenceState{State: "available"}
	for _, settlement := range settlements {
		if settlement.Binding.CandidateSHA == candidateSHA {
			snapshot.Verification = append(snapshot.Verification, VerificationState{
				State: string(settlement.State), CandidateSHA: settlement.Binding.CandidateSHA,
				PolicyRevision: settlement.Binding.PolicyRevision, Attempt: settlement.Binding.Attempt,
			})
		}
	}
}

func sameWorktreePath(left, right string) bool {
	if strings.TrimSpace(left) == "" || strings.TrimSpace(right) == "" {
		return false
	}
	normLeft, errLeft := normalizeWorktreePath(left)
	normRight, errRight := normalizeWorktreePath(right)
	return errLeft == nil && errRight == nil && normLeft == normRight
}

func normalizeWorktreePath(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("empty path")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(resolved), nil
	}
	dir := abs
	suffix := ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Clean(filepath.Join(resolved, suffix)), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return filepath.Clean(abs), nil
		}
		suffix = filepath.Join(filepath.Base(dir), suffix)
		dir = parent
	}
}

func printHuman(rows []Row, showFiles bool) {
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, r := range rows {
		line := fmt.Sprintf("%s\t%s\tahead=%d\tdirty=%d", r.Worktree, r.Branch, r.Ahead, r.Dirty)
		if r.Locked {
			line += "\tlocked"
		}
		line += "\t" + humanFleetState(r.Fleet)
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	if showFiles {
		for _, r := range rows {
			if len(r.Files) == 0 {
				continue
			}
			fmt.Printf("\n-- %s\n", r.Branch)
			for _, f := range r.Files {
				fmt.Printf("   %s\n", f)
			}
		}
	}
}

func humanFleetState(fleet FleetSnapshot) string {
	lease := "lease=unavailable"
	if fleet.Lease.State == "available" {
		lease = fmt.Sprintf("lease=available lane=%s owner=%s role=%s generation=%d", fleet.Lease.Lane, fleet.Lease.Owner, fleet.Lease.Role, fleet.Lease.Generation)
	}
	safeRef := "safe-ref=unavailable"
	if fleet.SafeRef.State == "available" {
		safeRef = fmt.Sprintf("safe-ref=available ref=%s sha=%s", fleet.SafeRef.Ref, fleet.SafeRef.SHA)
	}
	ci := "ci=unavailable"
	if fleet.CI.State == "available" {
		ci = "ci=unknown"
		if len(fleet.Verification) > 0 {
			states := make([]string, 0, len(fleet.Verification))
			for _, verification := range fleet.Verification {
				states = append(states, fmt.Sprintf("%s@%s policy=%s attempt=%d", verification.State, verification.CandidateSHA, verification.PolicyRevision, verification.Attempt))
			}
			ci = "ci=" + strings.Join(states, ",")
		}
	}
	return strings.Join([]string{lease, safeRef, "session=" + fleet.Session.State, "retention=" + fleet.Retention.State, ci}, " ")
}

func computeCollisions(rows []Row) []collision {
	// Map file path → set of branches
	pathBranches := make(map[string]map[string]bool)
	for _, r := range rows {
		for _, f := range r.Files {
			if pathBranches[f] == nil {
				pathBranches[f] = make(map[string]bool)
			}
			pathBranches[f][r.Branch] = true
		}
	}

	var cols []collision
	for f, branches := range pathBranches {
		if len(branches) < 2 {
			continue
		}
		sorted := make([]string, 0, len(branches))
		for b := range branches {
			sorted = append(sorted, b)
		}
		sort.Strings(sorted)
		cols = append(cols, collision{File: f, Branches: sorted})
	}
	sort.Slice(cols, func(i, j int) bool {
		return cols[i].File < cols[j].File
	})
	return cols
}

// countCherryAhead returns the number of patch-equivalent commits on HEAD
// that are not in base, using git cherry (not rev-list). This is critical:
// in a rebase-merge repo, reachability misreports already-merged branches
// as ahead.
func countCherryAhead(ctx context.Context, worktree, base string) int {
	out, err := gitOutput(ctx, worktree, "cherry", base, "HEAD")
	if err != nil {
		return 0
	}
	count := 0
	for _, line := range strings.Split(string(out), "\n") {
		if strings.HasPrefix(line, "+") {
			count++
		}
	}
	return count
}

// runGit runs a git command and returns an error if it fails.
func runGit(ctx context.Context, dir string, args ...string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Run()
}

// gitOutput runs a git command and returns its stdout.
func gitOutput(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	return cmd.Output()
}

// runGitOrDefault runs a git command and returns the default value on failure.
func runGitOrDefault(ctx context.Context, dir, def string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return def
	}
	return strings.TrimSpace(string(out))
}
