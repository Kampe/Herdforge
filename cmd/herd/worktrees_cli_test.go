package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/remoteci"
)

// ftpdHerdWorktreesRepo builds a hermetic repo with origin/main and two
// worktrees ahead of main, one touching a shared file.
func ftpdHerdWorktreesRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()

	// Init bare repo that looks like a remote
	runGitT(t, dir, "init", "--bare", "bare.git")

	// Clone into the "principal" repo (this is what herd worktrees scans)
	principal := filepath.Join(dir, "repo")
	runGitT(t, dir, "clone", "bare.git", "repo")
	// Empty clone lands on the git default branch (master under a clean
	// gitconfig); force main so `push origin main` has a branch to push.
	runGitT(t, principal, "checkout", "-b", "main")
	runGitT(t, principal, "config", "user.email", "t@kampe.kluster")
	runGitT(t, principal, "config", "user.name", "FAC-104 Test")
	runGitT(t, principal, "config", "commit.gpgSign", "false")
	runGitT(t, principal, "config", "tag.gpgSign", "false")
	writeRepoFile(t, principal, "README.md", "root\n")

	// Push main so origin/main exists
	runGitT(t, principal, "push", "origin", "main")

	// Create worktree branch: fac-101 (ahead by 2 commits, no collision)
	runGitT(t, principal, "branch", "herd/fac-101", "main")
	wt101 := filepath.Join(dir, "wt101")
	runGitT(t, principal, "worktree", "add", wt101, "herd/fac-101")
	runGitT(t, wt101, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt101, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt101, "config", "commit.gpgSign", "false")
	runGitT(t, wt101, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt101, "pkg/unique1.go", "package pkg\n// fac-101 unique\n")
	writeRepoFile(t, wt101, "pkg/unique2.go", "package pkg\n// fac-101 also\n")

	// Create worktree branch: fac-102 (ahead by 1, touches shared.go)
	runGitT(t, principal, "branch", "herd/fac-102", "main")
	wt102 := filepath.Join(dir, "wt102")
	runGitT(t, principal, "worktree", "add", wt102, "herd/fac-102")
	runGitT(t, wt102, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt102, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt102, "config", "commit.gpgSign", "false")
	runGitT(t, wt102, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt102, "pkg/shared.go", "package pkg\n// fac-102\n")

	// Create worktree branch: fac-103 (dirty, touches shared.go differently)
	runGitT(t, principal, "branch", "herd/fac-103", "main")
	wt103 := filepath.Join(dir, "wt103")
	runGitT(t, principal, "worktree", "add", wt103, "herd/fac-103")
	runGitT(t, wt103, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt103, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt103, "config", "commit.gpgSign", "false")
	runGitT(t, wt103, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt103, "pkg/shared.go", "package pkg\n// fac-103\n")
	// Dirty modification on top
	if err := os.WriteFile(filepath.Join(wt103, "pkg/dirty.go"), []byte("package pkg\n// dirty\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create a locked worktree
	runGitT(t, principal, "branch", "herd/locked", "main")
	wtLocked := filepath.Join(dir, "wtLocked")
	runGitT(t, principal, "worktree", "add", wtLocked, "herd/locked")
	runGitT(t, principal, "worktree", "lock", wtLocked)

	return principal
}

func TestHerdWorktreesCLIBasic(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 3 for collisions, got success")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("expected exit code 3, got %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "ahead=") {
		t.Errorf("expected ahead= column in output, got:\n%s", s)
	}
	if !strings.Contains(s, "dirty=") {
		t.Errorf("expected dirty= column in output, got:\n%s", s)
	}
	if !strings.Contains(s, "COLLISIONS:") {
		t.Errorf("expected COLLISIONS section, got:\n%s", s)
	}
	if !strings.Contains(s, "pkg/shared.go") {
		t.Errorf("expected pkg/shared.go in collisions, got:\n%s", s)
	}
}

func TestHerdWorktreesCLIHumanReportsUnavailableFleetEvidence(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected collision exit")
	}
	for _, want := range []string{"lease=unavailable", "safe-ref=unavailable", "session=unavailable", "retention=unavailable", "ci=unavailable"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("human snapshot missing %q:\n%s", want, out)
		}
	}
}

func TestHerdWorktreesCLIJSON(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for --json, got %v\n%s", err, out)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	if snapshot.SchemaVersion != worktreesSchemaVersion {
		t.Fatalf("schema version = %d, want %d", snapshot.SchemaVersion, worktreesSchemaVersion)
	}
	rows := snapshot.Worktrees
	if len(rows) == 0 {
		t.Fatal("expected at least one row")
	}
	foundShared := false
	for _, r := range rows {
		if r.Branch == "" {
			t.Error("every row must have a branch")
		}
		if r.Worktree == "" {
			t.Error("every row must have a worktree path")
		}
		if r.Head == "" || r.Head == "?" {
			t.Error("every row must have a non-empty HEAD")
		}
		if r.Fleet.Lease.State != "unavailable" || r.Fleet.SafeRef.State != "unavailable" || r.Fleet.Session.State != "unavailable" || r.Fleet.Retention.State != "unavailable" || r.Fleet.CI.State != "unavailable" {
			t.Errorf("missing fleet sources must be explicit unavailable: %+v", r.Fleet)
		}
		for _, f := range r.Files {
			if f == "pkg/shared.go" {
				foundShared = true
			}
		}
	}
	if !foundShared {
		t.Errorf("expected shared.go in files, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIFiles(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--files")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 3 for collisions")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 3 {
		t.Fatalf("expected exit code 3, got %v\n%s", err, out)
	}
	s := string(out)
	// Each branch with files should have a -- heading
	if !strings.Contains(s, "-- herd/fac-101") {
		t.Errorf("expected -- herd/fac-101 files section, got:\n%s", s)
	}
	if !strings.Contains(s, "pkg/unique1.go") {
		t.Errorf("expected unique1.go in files, got:\n%s", s)
	}
}

func TestHerdWorktreesCLIJSONFiles(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--json", "--files")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for --json --files, got %v\n%s", err, out)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("expected valid JSON: %v\n%s", err, out)
	}
	rows := snapshot.Worktrees
	// --files flag should be ignored when --json is set; files are always included in JSON
	if len(rows) == 0 {
		t.Fatal("expected rows")
	}
}

func TestHerdWorktreesCLIHelp(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--help")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for --help, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-worktrees") {
		t.Errorf("expected herd-worktrees header, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIUnknownArg(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--bogus")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 2 for unknown arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
}

func TestHerdWorktreesCLINoOriginMain(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()
	runGitT(t, dir, "init", "-b", "main")
	runGitT(t, dir, "config", "user.email", "t@kampe.kluster")
	runGitT(t, dir, "config", "user.name", "FAC-104 Test")
	runGitT(t, dir, "config", "commit.gpgSign", "false")
	runGitT(t, dir, "config", "tag.gpgSign", "false")
	writeRepoFile(t, dir, "README.md", "root\n")

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 1 without origin/main")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		t.Fatalf("expected exit code 1, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "origin/main not found") {
		t.Errorf("expected origin/main not found, got:\n%s", out)
	}
}

func TestHerdWorktreesCLIVanishedWorktree(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	// Find a worktree path and delete its directory to simulate vanished
	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("initial worktrees failed: %v\n%s", err, out)
	}
	var beforeSnapshot Snapshot
	if err := json.Unmarshal(out, &beforeSnapshot); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	before := beforeSnapshot.Worktrees

	// Delete the first non-principal worktree directory
	for _, r := range before {
		if r.Branch != "main" && r.Worktree != dir {
			if err := os.RemoveAll(r.Worktree); err != nil {
				t.Fatal(err)
			}
			break
		}
	}

	// Re-run should not crash, should have fewer rows
	cmd2 := exec.Command(binary, "worktrees", "--json")
	cmd2.Dir = dir
	out2, err := cmd2.CombinedOutput()
	if err != nil {
		t.Fatalf("second worktrees failed: %v\n%s", err, out2)
	}
	var afterSnapshot Snapshot
	if err := json.Unmarshal(out2, &afterSnapshot); err != nil {
		t.Fatalf("invalid JSON after vanished: %v\n%s", err, out2)
	}
	after := afterSnapshot.Worktrees
	if len(after) >= len(before) {
		t.Errorf("expected fewer rows after deleting a worktree dir: before=%d after=%d", len(before), len(after))
	}
}

func TestHerdWorktreesCLILocked(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktrees failed: %v\n%s", err, out)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	rows := snapshot.Worktrees
	foundLocked := false
	for _, r := range rows {
		if r.Locked {
			foundLocked = true
			break
		}
	}
	if !foundLocked {
		t.Error("expected a locked worktree in output")
	}
}

func TestHerdWorktreesCLINoCollisions(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()

	// Build a repo with one worktree ahead but no shared files
	runGitT(t, dir, "init", "--bare", "bare.git")

	principal := filepath.Join(dir, "repo")
	runGitT(t, dir, "clone", "bare.git", "repo")
	// Empty clone lands on the git default branch (master under a clean
	// gitconfig); force main so `push origin main` has a branch to push.
	runGitT(t, principal, "checkout", "-b", "main")
	runGitT(t, principal, "config", "user.email", "t@kampe.kluster")
	runGitT(t, principal, "config", "user.name", "FAC-104 Test")
	runGitT(t, principal, "config", "commit.gpgSign", "false")
	runGitT(t, principal, "config", "tag.gpgSign", "false")
	writeRepoFile(t, principal, "README.md", "root\n")
	runGitT(t, principal, "push", "origin", "main")

	runGitT(t, principal, "branch", "herd/fac-x", "main")
	wt := filepath.Join(dir, "wtx")
	runGitT(t, principal, "worktree", "add", wt, "herd/fac-x")
	runGitT(t, wt, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt, "config", "commit.gpgSign", "false")
	runGitT(t, wt, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt, "pkg/only_me.go", "package pkg\n// only on this branch\n")

	cmd := exec.Command(binary, "worktrees")
	cmd.Dir = principal
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for no collisions, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "COLLISIONS: none") {
		t.Errorf("expected COLLISIONS: none, got:\n%s", out)
	}
}

func TestHerdWorktreesJSONFleetEvidenceIsExactAndUnavailableIsExplicit(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)
	wt101 := filepath.Join(filepath.Dir(dir), "wt101")
	candidateOut, err := exec.Command("git", "-C", wt101, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	candidate := strings.TrimSpace(string(candidateOut))

	leaseStore, err := claim.NewSQLiteLeaseStore(filepath.Join(dir, ".herd", "herdforge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer leaseStore.Close()
	if _, err := leaseStore.Acquire(context.Background(), claim.LeaseKey{Repo: "repo", Provider: "kaneo", Project: "project", TaskRef: "FAC-101"}, "owner-101", "worker", wt101, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}

	ciStore, err := remoteci.Open(filepath.Join(dir, ".herd", "remote-ci.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	match := remoteci.Binding{Repository: "repo", CandidateSHA: candidate, PolicyRevision: "policy-a", Attempt: 1, RequiredChecks: []string{"unit"}}
	if _, _, err := ciStore.Register(match); err != nil {
		t.Fatal(err)
	}
	if _, err := ciStore.PersistTerminal(remoteci.Settlement{Version: remoteci.Version1, Binding: match, State: remoteci.StatePassed}); err != nil {
		t.Fatal(err)
	}
	mismatch := remoteci.Binding{Repository: "repo", CandidateSHA: strings.Repeat("a", 40), PolicyRevision: "policy-b", Attempt: 1, RequiredChecks: []string{"unit"}}
	if _, _, err := ciStore.Register(mismatch); err != nil {
		t.Fatal(err)
	}
	if _, err := ciStore.PersistTerminal(remoteci.Settlement{Version: remoteci.Version1, Binding: mismatch, State: remoteci.StatePassed}); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktrees --json: %v\n%s", err, out)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("decode snapshot: %v\n%s", err, out)
	}
	var matched, other *Row
	for i := range snapshot.Worktrees {
		row := &snapshot.Worktrees[i]
		switch row.Branch {
		case "herd/fac-101":
			matched = row
		case "herd/fac-102":
			other = row
		}
	}
	if matched == nil || other == nil {
		t.Fatalf("expected fixture rows, got %+v", snapshot.Worktrees)
	}
	if matched.CandidateSHA != candidate || len(matched.Fleet.Verification) != 1 || matched.Fleet.Verification[0].State != string(remoteci.StatePassed) || matched.Fleet.Verification[0].CandidateSHA != candidate {
		t.Fatalf("matching candidate did not retain exact CI evidence: %+v", matched)
	}
	if matched.Fleet.Lease.State != "available" || matched.Fleet.Lease.Owner != "owner-101" || matched.Fleet.Lease.Role != "worker" {
		t.Fatalf("matching worktree did not retain lease ownership: %+v", matched.Fleet.Lease)
	}
	if len(other.Fleet.Verification) != 0 {
		t.Fatalf("different candidate inherited CI evidence: %+v", other.Fleet.Verification)
	}
	if other.Fleet.CI.State != "available" {
		t.Fatalf("present CI ledger must be available even when no settlement matches: %+v", other.Fleet)
	}
	if other.Fleet.Session.State != "unavailable" || other.Fleet.Retention.State != "unavailable" || other.Fleet.SafeRef.State != "unavailable" {
		t.Fatalf("unreadable evidence must remain unavailable: %+v", other.Fleet)
	}
}

func TestHerdWorktreesJSONOrderIsDeterministic(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)
	run := func() []byte {
		cmd := exec.Command(binary, "worktrees", "--json")
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("worktrees --json: %v\n%s", err, out)
		}
		return out
	}
	first, second := run(), run()
	if string(first) != string(second) {
		t.Fatalf("JSON output is nondeterministic:\nfirst=%s\nsecond=%s", first, second)
	}
}

func TestHerdWorktreesCLIUnknownPositionalArg(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "extra-arg")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 2 for unknown positional arg")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "unknown arg") {
		t.Errorf("expected 'unknown arg', got:\n%s", out)
	}
}

// TestHerdWorktreesCLIJSONFilesEmptyArray verifies that a clean worktree
// (no ahead commits, no dirty files) serialises files as [] not null in
// JSON output.  A nil slice marshals to null in Go, which breaks every
// JSON consumer expecting an array.
func TestHerdWorktreesCLIJSONFilesEmptyArray(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()

	runGitT(t, dir, "init", "--bare", "bare.git")
	principal := filepath.Join(dir, "repo")
	runGitT(t, dir, "clone", "bare.git", "repo")
	runGitT(t, principal, "checkout", "-b", "main")
	runGitT(t, principal, "config", "user.email", "t@kampe.kluster")
	runGitT(t, principal, "config", "user.name", "FAC-104 Test")
	runGitT(t, principal, "config", "commit.gpgSign", "false")
	runGitT(t, principal, "config", "tag.gpgSign", "false")
	writeRepoFile(t, principal, "README.md", "root\n")
	runGitT(t, principal, "push", "origin", "main")

	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = principal
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktrees --json failed: %v\n%s", err, out)
	}

	// The raw JSON must contain "files": [] not "files": null.  We check
	// the raw bytes because json.Unmarshal into Snapshot would paper over
	// the difference (a null slice stays nil, which len()==0 hides).
	raw := string(out)
	if strings.Contains(raw, `"files": null`) {
		t.Errorf("files field is null, expected []\n%s", raw)
	}
	if !strings.Contains(raw, `"files": []`) {
		t.Errorf("expected files: [] in JSON, got:\n%s", raw)
	}

	// Also verify via typed unmarshal that the row is well-formed.
	var snapshot Snapshot
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if snapshot.SchemaVersion != worktreesSchemaVersion {
		t.Fatalf("schema version = %d, want %d", snapshot.SchemaVersion, worktreesSchemaVersion)
	}
	rows := snapshot.Worktrees
	if len(rows) == 0 {
		t.Fatal("expected at least the principal worktree row")
	}
	for _, r := range rows {
		if r.Files == nil {
			t.Errorf("row branch=%s has nil Files, expected empty slice", r.Branch)
		}
		if r.Fleet.Lease.State != "unavailable" || r.Fleet.SafeRef.State != "unavailable" || r.Fleet.Session.State != "unavailable" || r.Fleet.Retention.State != "unavailable" || r.Fleet.CI.State != "unavailable" {
			t.Errorf("missing fleet sources must be explicit unavailable: %+v", r.Fleet)
		}
	}
}

// TestHerdWorktreesCLISingleDashHelp verifies that -help (single dash,
// a valid Go flag synonym) exits 0 like --help.  Previously the pre-parse
// loop only checked --help and -h; -help fell through to the flag parser
// which returned flag.ErrHelp, and the error path exited 2.
func TestHerdWorktreesCLISingleDashHelp(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "-help")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("expected exit 0 for -help, got %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "herd-worktrees") {
		t.Errorf("expected herd-worktrees header, got:\n%s", out)
	}
}

// TestHerdWorktreesCLIUnknownFlagMessage verifies that an unknown flag
// produces the spec-contracted stderr message "herd-worktrees: unknown
// arg X" — not the Go flag package's default "flag provided but not
// defined: -X".
func TestHerdWorktreesCLIUnknownFlagMessage(t *testing.T) {
	binary := buildHerd(t)
	dir := ftpdHerdWorktreesRepo(t)

	cmd := exec.Command(binary, "worktrees", "--bogus")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("expected exit 2 for unknown flag")
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 {
		t.Fatalf("expected exit code 2, got %v\n%s", err, out)
	}
	s := string(out)
	if !strings.Contains(s, "herd-worktrees: unknown arg --bogus") {
		t.Errorf("expected 'herd-worktrees: unknown arg --bogus', got:\n%s", s)
	}
	// The flag package's default message must NOT appear.
	if strings.Contains(s, "flag provided but not defined") {
		t.Errorf("flag package default message leaked, got:\n%s", s)
	}
}

// TestHerdWorktreesCLIRebasedBranchAheadZero verifies that a branch whose
// commits are patch-equivalent to origin/main (e.g. after a rebase-merge)
// reports ahead=0.  This uses git cherry, not rev-list, because
// reachability would misreport already-merged branches as ahead.
func TestHerdWorktreesCLIRebasedBranchAheadZero(t *testing.T) {
	binary := buildHerd(t)
	dir := t.TempDir()

	runGitT(t, dir, "init", "--bare", "bare.git")
	principal := filepath.Join(dir, "repo")
	runGitT(t, dir, "clone", "bare.git", "repo")
	runGitT(t, principal, "checkout", "-b", "main")
	runGitT(t, principal, "config", "user.email", "t@kampe.kluster")
	runGitT(t, principal, "config", "user.name", "FAC-104 Test")
	runGitT(t, principal, "config", "commit.gpgSign", "false")
	runGitT(t, principal, "config", "tag.gpgSign", "false")
	writeRepoFile(t, principal, "README.md", "root\n")
	runGitT(t, principal, "push", "origin", "main")

	// Create a branch with one commit that touches a unique file.
	runGitT(t, principal, "branch", "herd/fac-rebase", "main")
	wt := filepath.Join(dir, "wt-rebase")
	runGitT(t, principal, "worktree", "add", wt, "herd/fac-rebase")
	runGitT(t, wt, "config", "user.email", "t@kampe.kluster")
	runGitT(t, wt, "config", "user.name", "FAC-104 Test")
	runGitT(t, wt, "config", "commit.gpgSign", "false")
	runGitT(t, wt, "config", "tag.gpgSign", "false")
	writeRepoFile(t, wt, "pkg/rebased.go", "package pkg\n// rebased\n")

	// Cherry-pick that commit onto main so the patches are equivalent.
	branchHead := strings.TrimSpace(runGitT(t, wt, "rev-parse", "HEAD"))
	runGitT(t, principal, "cherry-pick", branchHead)
	runGitT(t, principal, "push", "origin", "main")

	// Now the branch's commit is patch-equivalent to a commit on
	// origin/main.  git cherry should report 0 ahead.
	cmd := exec.Command(binary, "worktrees", "--json")
	cmd.Dir = principal
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("worktrees --json failed: %v\n%s", err, out)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(out, &snapshot); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, out)
	}
	if snapshot.SchemaVersion != worktreesSchemaVersion {
		t.Fatalf("schema version = %d, want %d", snapshot.SchemaVersion, worktreesSchemaVersion)
	}
	for _, r := range snapshot.Worktrees {
		if r.Branch == "herd/fac-rebase" {
			if r.Ahead != 0 {
				t.Errorf("rebased branch should have ahead=0 (patch-equivalent), got ahead=%d", r.Ahead)
			}
			if r.Fleet.Lease.State != "unavailable" || r.Fleet.SafeRef.State != "unavailable" || r.Fleet.Session.State != "unavailable" || r.Fleet.Retention.State != "unavailable" || r.Fleet.CI.State != "unavailable" {
				t.Errorf("missing fleet sources must be explicit unavailable: %+v", r.Fleet)
			}
			return
		}
	}
	t.Fatal("herd/fac-rebase branch not found in worktree rows")
}
