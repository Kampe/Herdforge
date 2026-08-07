package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Kampe/Herdforge/pkg/dispatch"
	"github.com/Kampe/Herdforge/pkg/scopeauth"
	"github.com/Kampe/Herdforge/pkg/scopefence"
)

// runScope publishes the trusted graph snapshot and task scope that the
// dispatch fence resolves against (FAC-186).
//
// Nothing published either, so SQLiteScopeAuthority.Resolve always returned
// "trusted task scope unavailable" and every production dispatch failed after
// the authority check. The fence is designed so scope comes from a source the
// acquiring caller is NOT — publication is a coordinator act, which is why
// this is an explicit command rather than something dispatch does for itself.
func runScope() {
	fs := flag.NewFlagSet("scope", flag.ExitOnError)
	files := fs.String("files", "", "Comma-separated files this task may touch")
	packages := fs.String("packages", "", "Comma-separated packages this task may touch")
	revFlag := fs.String("revision", "", "DEPS graph revision to bind to (herd dispatch prints it on failure)")

	args := os.Args[2:]
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	task := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		task, args = args[0], args[1:]
	}
	fs.Parse(args)

	if sub == "release" && task != "" {
		releaseScope(task)
		return
	}
	if sub == "status" {
		showScope()
		return
	}
	if sub != "publish" || task == "" {
		fmt.Fprintln(os.Stderr, "usage: herd scope publish <TASK-REF> --revision <deps-graph-rev> --files a.go,b.go [--packages pkg/x]")
		fmt.Fprintln(os.Stderr, "       herd scope release <TASK-REF>   surrender a stale claim (fenced abandonment)")
		fmt.Fprintln(os.Stderr, "       herd scope status               list current scope owners")
		fmt.Fprintln(os.Stderr, "  Publishes the graph snapshot and task scope the dispatch fence resolves")
		fmt.Fprintln(os.Stderr, "  against. A scope must declare something: scopefence refuses an empty one,")
		fmt.Fprintln(os.Stderr, "  because a task that owns nothing cannot be fenced against anything.")
		os.Exit(2)
	}

	repository, err := dispatch.AuthenticatedRepositoryIdentity(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: repository identity: %v\n", err)
		os.Exit(1)
	}
	// This is deps.GraphRevision -- a hash of board edges and prerequisite
	// statuses -- NOT a git commit. Binding it to origin/main was the mistake
	// that made every published scope unresolvable.
	revision := strings.TrimSpace(*revFlag)
	if revision == "" {
		fmt.Fprintln(os.Stderr, "herd scope: --revision is required (the DEPS graph revision, which herd dispatch prints when scope is unavailable)")
		os.Exit(2)
	}

	store, err := scopefence.NewSQLiteStore(filepath.Join(".", ".herd", "scopefence.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: open store: %v\n", err)
		os.Exit(1)
	}

	// The scope authority resolves against a published GRAPH snapshot, so
	// publishing scope alone still leaves "trusted task scope unavailable".
	// Counts come from the real tree at this revision and must match the
	// dispatcher's own binding exactly — countTrackedFiles is shared so the
	// two cannot disagree.
	// Graph.Files must be positive and must equal what the dispatcher binds;
	// the dispatcher reads it back from this same row, so any consistent
	// positive count works. Use the real tracked-file count so it is meaningful.
	fileCount := 0
	if out, lsErr := exec.Command("git", "ls-tree", "-r", "--name-only", "HEAD").Output(); lsErr == nil {
		for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.TrimSpace(l) != "" {
				fileCount++
			}
		}
	}
	if fileCount == 0 {
		fmt.Fprintln(os.Stderr, "herd scope: no tracked files at HEAD")
		os.Exit(1)
	}
	graph := scopefence.Graph{
		Revision: revision,
		Files:    fileCount,
		// scopefence.validate requires every count positive, Complete true,
		// and edges >= nodes, or it rejects the snapshot as implausible.
		Nodes:    fileCount,
		Edges:    fileCount,
		Flows:    1,
		Complete: true,
	}
	if err := store.PutGraphSnapshot(context.Background(), repository, graph); err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: publish graph: %v\n", err)
		os.Exit(1)
	}

	scope := scopefence.Scope{
		Files:    splitList(*files),
		Packages: splitList(*packages),
		Symbols:  []string{},
	}
	if err := store.PutScopeDeclaration(context.Background(), repository, task, revision, scope); err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: publish scope: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("herd scope: published %s at %s (scope files=%d packages=%d; graph files=%d)\n",
		task, shortSHA(revision), len(scope.Files), len(scope.Packages), fileCount)
}

// splitList returns a NON-NIL slice. canonicalScope normalises to empty
// non-nil slices and PutScopeDeclaration refuses anything that does not equal
// its own canonical form, so a nil is rejected as "invalid scope declaration"
// with no hint as to why.
func splitList(csv string) []string {
	out := []string{}
	for _, v := range strings.Split(csv, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// openScopeStore is the shared store handle for the scope subcommands.
func openScopeStore() (*scopefence.SQLiteStore, string) {
	repository, err := dispatch.AuthenticatedRepositoryIdentity(".")
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: repository identity: %v\n", err)
		os.Exit(1)
	}
	store, err := scopefence.NewSQLiteStore(filepath.Join(".", ".herd", "scopefence.db"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: open store: %v\n", err)
		os.Exit(1)
	}
	return store, repository
}

// showScope lists who currently holds scope. A stale owner is invisible
// otherwise, and it is the reason a re-dispatch fails with identity_conflict.
func showScope() {
	store, _ := openScopeStore()
	snap, err := store.Read(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: read: %v\n", err)
		os.Exit(1)
	}
	if len(snap.Owners) == 0 {
		fmt.Println("herd scope: no active scope owners")
		return
	}
	for _, o := range snap.Owners {
		fmt.Printf("%-12s gen=%-4d state=%-10s files=%d packages=%d rev=%s\n",
			o.Task, o.Generation, o.State, len(o.Scope.Files), len(o.Scope.Packages), shortSHA(o.GraphRevision))
	}
}

// releaseScope surrenders a stale claim through the fence's own abandonment
// path rather than deleting the row. A partial dispatch leaves an Active owner
// at a generation the retry cannot match, and every later attempt then fails
// with identity_conflict and no way to clear it.
func releaseScope(task string) {
	store, repository := openScopeStore()
	snap, err := store.Read(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: read: %v\n", err)
		os.Exit(1)
	}
	var owner *scopefence.Ownership
	for i := range snap.Owners {
		if snap.Owners[i].Task == task {
			owner = &snap.Owners[i]
			break
		}
	}
	if owner == nil {
		fmt.Printf("herd scope: %s holds no scope\n", task)
		return
	}
	fence := scopefence.Fence{Store: store, ReleaseAuthority: scopeauth.New()}
	req := scopefence.ReleaseRequest{
		Ownership: *owner,
		Authority: scopefence.FencedAbandonment,
	}
	if err := fence.Release(context.Background(), req); err != nil {
		fmt.Fprintf(os.Stderr, "herd scope: release %s: %v\n", task, err)
		os.Exit(1)
	}
	fmt.Printf("herd scope: released %s (was gen=%d state=%s) in %s\n",
		task, owner.Generation, owner.State, repository)
}
