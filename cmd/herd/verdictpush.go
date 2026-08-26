package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
)

var (
	transportedReviewerAgents = herdr.AgentList
	transportedReviewerClose  = herdr.CloseSettledReviewTab
	transportedReviewerHead   = func(cwd string) (string, error) {
		out, err := exec.Command("git", "-C", cwd, "rev-parse", "HEAD").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("candidate HEAD: %s", strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out)), nil
	}
)

type reviewReapReceipt struct {
	ObservedAt  string `json:"observed_at"`
	Layer       string `json:"layer"`
	Disposition string `json:"disposition"`
	Reason      string `json:"reason,omitempty"`
	Artifact    string `json:"artifact"`
	SHA         string `json:"sha,omitempty"`
	Reviewer    string `json:"reviewer,omitempty"`
	TabID       string `json:"tab_id,omitempty"`
	PaneID      string `json:"pane_id,omitempty"`
}

// runVerdictPush transports one verdict artifact to the ledger host over git.
//
// FAC-619: this replaces a THREE-STEP git recipe in the review packet that could
// not work, for two reasons found only by running it:
//
//  1. .gitignore line 92 ignores /.herd/*, so `git add <verdict>` silently
//     no-ops. The reviewer sees "nothing to commit, working tree clean" and
//     reasonably concludes it already reported.
//  2. verdicts/<ws> is a long-lived branch that has diverged from the reviewer's
//     HEAD, so pushing HEAD to it is rejected as a non-fast-forward.
//
// Three previous report-home mechanisms shipped broken because each was checked
// by reading the rendered packet instead of executing it. A multi-step git recipe
// handed to a reviewer is a fourth chance to get it wrong, so this is one
// command that Herdforge owns and can be tested.
//
// It uses plumbing only -- hash-object, mktree, commit-tree -- so it never
// checks out, stashes, or switches a branch in the reviewer's worktree. A review
// host must not have its tree mutated to send a message.
func runVerdictPush() error {
	fs := flag.NewFlagSet("verdict-push", flag.ContinueOnError)
	artifact := fs.String("artifact", "", "Path to the verdict artifact to transport")
	sweep := fs.Bool("sweep", false,
		"Push every local verdict that is not yet on the remote, instead of one named artifact")
	workspace := fs.String("workspace", "", "Workspace id (default: resolved from config/env)")
	remote := fs.String("remote", "origin", "Git remote to push to")
	dryRun := fs.Bool("dry-run", false, "Build the commit and report the ref without pushing")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	if *sweep {
		// FAC-638: do not depend on the reviewer remembering to push.
		//
		// Measured on the review host: 88 verdicts written locally, 84 refs on the
		// remote. SEVEN completed reviews existed only on that host's disk, so the
		// ledger never saw them and the supervisor had to poll for something that
		// was never coming. Reviewers were doing the work and dropping the last
		// step -- which is a predictable outcome of making delivery the reviewer's
		// responsibility rather than the system's.
		//
		// Sweeping is idempotent and safe to schedule: a verdict already on the
		// remote is skipped, so running it every few minutes converts "the reviewer
		// remembered" into "the transport happened".
		return sweepVerdictPush(*remote, strings.TrimSpace(*workspace), *dryRun)
	}
	path := strings.TrimSpace(*artifact)
	if path == "" {
		return fmt.Errorf("--artifact is required, or use --sweep (usage: herd verdict-push --artifact <path> | --sweep)")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("verdict artifact %q is not a readable file", path)
	}
	if info.Size() == 0 {
		return fmt.Errorf("verdict artifact %q is empty; refusing to transport a blank verdict", path)
	}

	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	ws := strings.TrimSpace(*workspace)
	if ws == "" {
		if resolved, wsErr := herdr.RequireWorkspace(root); wsErr == nil {
			ws = strings.TrimSpace(resolved)
		} else {
			ws = strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
		}
	}
	if ws == "" {
		return fmt.Errorf("cannot resolve a workspace id; pass --workspace so the verdicts ref is not empty")
	}

	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	// -w writes the blob regardless of .gitignore: plumbing does not consult it.
	// This is precisely the step the packet recipe could not express.
	blob, err := git("hash-object", "-w", path)
	if err != nil {
		return fmt.Errorf("hash verdict artifact: %s", blob)
	}
	leaf := filepath.Base(path)
	tree, err := gitMkTree(root, blob, leaf)
	if err != nil {
		return err
	}

	// Each verdict gets its OWN ref, so two reviewers finishing at once cannot
	// reject each other with a non-fast-forward. The harvester already fetches
	// refs/heads/verdicts/* as a glob.
	ref := fmt.Sprintf("refs/heads/verdicts/%s-%s", ws, strings.TrimSuffix(leaf, ".md"))
	if len(ref) > 220 {
		ref = ref[:220]
	}
	commit, err := git("commit-tree", tree, "-m", "verdict: "+leaf)
	if err != nil {
		return fmt.Errorf("create verdict commit: %s", commit)
	}
	if *dryRun {
		fmt.Printf("verdict-push DRY RUN commit=%s ref=%s artifact=%s\n", shortSHA(commit), ref, leaf)
		return nil
	}
	if out, pushErr := git("push", "--force", *remote, commit+":"+ref); pushErr != nil {
		return fmt.Errorf("push verdict ref %s: %s", ref, out)
	}
	// A push that exits 0 is not proof the ref landed. Read it back.
	out, err := git("ls-remote", "--heads", *remote, strings.TrimPrefix(ref, "refs/heads/"))
	if err != nil || strings.TrimSpace(out) == "" {
		return fmt.Errorf("push reported success but %s is not present on %s", ref, *remote)
	}
	fmt.Printf("verdict-push OK ref=%s commit=%s artifact=%s\n", ref, shortSHA(commit), leaf)
	return nil
}

// gitMkTree builds a single-entry tree placing the artifact under the canonical
// inbox path, so the harvester finds it exactly where it expects.
func gitMkTree(root, blob, leaf string) (string, error) {
	entry := fmt.Sprintf("100644 blob %s\t%s\n", blob, leaf)
	inner := exec.Command("git", "-C", root, "mktree")
	inner.Stdin = strings.NewReader(entry)
	innerOut, err := inner.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mktree verdict leaf: %s", strings.TrimSpace(string(innerOut)))
	}
	innerTree := strings.TrimSpace(string(innerOut))

	// Nest it as .herd/review/inbox/<leaf>.
	tree := innerTree
	for _, dir := range []string{"inbox", "review", ".herd"} {
		e := fmt.Sprintf("040000 tree %s\t%s\n", tree, dir)
		c := exec.Command("git", "-C", root, "mktree")
		c.Stdin = strings.NewReader(e)
		o, mkErr := c.CombinedOutput()
		if mkErr != nil {
			return "", fmt.Errorf("mktree %s: %s", dir, strings.TrimSpace(string(o)))
		}
		tree = strings.TrimSpace(string(o))
	}
	return tree, nil
}

// sweepVerdictPush pushes every local verdict artifact that has no ref on the
// remote yet.
//
// FAC-638: this exists because delivery was the reviewer's job and reviewers
// dropped it 7 times out of 88. A step that must be remembered is a step that is
// sometimes skipped; a sweep makes the transport a property of the system.
func sweepVerdictPush(remote, workspace string, dryRun bool) error {
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		if resolved, err := herdr.RequireWorkspace(root); err == nil {
			ws = strings.TrimSpace(resolved)
		} else {
			ws = strings.TrimSpace(os.Getenv("HERD_WORKSPACE"))
		}
	}
	if ws == "" {
		return fmt.Errorf("cannot resolve a workspace id; pass --workspace so verdict refs are not written under an empty name")
	}

	inbox := filepath.Join(root, ".herd", "review", "inbox")
	entries, err := os.ReadDir(inbox)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("verdict-push sweep: no inbox; nothing to transport")
			return nil
		}
		return fmt.Errorf("read inbox %s: %w", inbox, err)
	}

	// One remote listing, not one per artifact: 88 artifacts would otherwise mean
	// 88 network round trips.
	existing := map[string]bool{}
	out, lsErr := exec.Command("git", "-C", root, "ls-remote", "--heads", remote, "verdicts/*").Output()
	if lsErr != nil {
		// Fail closed: an unreachable remote is not proof that nothing is pushed,
		// and assuming so would re-push everything on every sweep.
		return fmt.Errorf("list remote verdict refs: %w", lsErr)
	}
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "refs/heads/"); i >= 0 {
			existing[strings.TrimSpace(line[i+len("refs/heads/"):])] = true
		}
	}

	pushed, skipped, failed, reaped, reapFailed := 0, 0, 0, 0, 0
	var residentAgents []herdr.AgentEntry
	var residentCensusErr error
	residentCensusLoaded := false
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		ref := fmt.Sprintf("verdicts/%s-%s", ws, strings.TrimSuffix(name, ".md"))
		if len(ref) > 220 {
			ref = ref[:220]
		}
		transported := existing[ref]
		if transported {
			skipped++
		} else if dryRun {
			fmt.Printf("WOULD_PUSH %s -> %s\n", name, ref)
			pushed++
		} else {
			os.Args = []string{"herd", "verdict-push", "--artifact", filepath.Join(inbox, name), "--workspace", ws, "--remote", remote}
			if err := runVerdictPush(); err != nil {
				fmt.Fprintf(os.Stderr, "verdict-push sweep: %s: %v\n", name, err)
				failed++
				continue
			}
			pushed++
			transported = true // runVerdictPush performed its own ls-remote readback.
		}

		if transported || dryRun {
			if !residentCensusLoaded {
				residentAgents, residentCensusErr = transportedReviewerAgents()
				residentCensusLoaded = true
			}
			receipt, reapErr := reapTransportedReviewerFromCensus(root, filepath.Join(inbox, name), dryRun, residentAgents, residentCensusErr)
			if !dryRun {
				if err := appendReviewReapReceipt(root, receipt); err != nil && reapErr == nil {
					reapErr = fmt.Errorf("persist cleanup receipt: %w", err)
					receipt.Disposition, receipt.Reason = "blocked", reapErr.Error()
				}
			}
			if receipt.Disposition == "reaped" {
				reaped++
				residentAgents = removeResidentAgent(residentAgents, receipt.Reviewer)
			}
			if reapErr != nil {
				reapFailed++
				fmt.Fprintf(os.Stderr, "REAP_BLOCKED layer=%s artifact=%s reason=%q\n", receipt.Layer, name, receipt.Reason)
			} else if receipt.Disposition == "would_reap" {
				fmt.Printf("WOULD_REAP reviewer=%s sha=%s tab=%s\n", receipt.Reviewer, shortSHA(receipt.SHA), receipt.TabID)
			}
		}
	}
	fmt.Printf("verdict-push sweep: pushed=%d already_present=%d failed=%d reaped=%d reap_failed=%d workspace=%s\n",
		pushed, skipped, failed, reaped, reapFailed, ws)
	if failed > 0 || reapFailed > 0 {
		return fmt.Errorf("transport_failed=%d resident_cleanup_failed=%d", failed, reapFailed)
	}
	return nil
}

func removeResidentAgent(agents []herdr.AgentEntry, name string) []herdr.AgentEntry {
	out := agents[:0]
	for _, agent := range agents {
		if agent.Name != name {
			out = append(out, agent)
		}
	}
	return out
}

func reapTransportedReviewer(root, artifactPath string, dryRun bool) (reviewReapReceipt, error) {
	agents, censusErr := transportedReviewerAgents()
	return reapTransportedReviewerFromCensus(root, artifactPath, dryRun, agents, censusErr)
}

func reapTransportedReviewerFromCensus(root, artifactPath string, dryRun bool, agents []herdr.AgentEntry, censusErr error) (reviewReapReceipt, error) {
	r := reviewReapReceipt{
		ObservedAt: time.Now().UTC().Format(time.RFC3339Nano), Layer: "resident_cleanup",
		Disposition: "retained", Artifact: filepath.Base(artifactPath),
	}
	body, err := os.ReadFile(artifactPath)
	if err != nil {
		r.Disposition, r.Reason = "blocked", "read verdict artifact: "+err.Error()
		return r, err
	}
	a := reviewingest.Parse(string(body))
	r.SHA, r.Reviewer = strings.TrimSpace(a.SHA), strings.TrimSpace(a.Reviewer)
	if len(r.SHA) != 40 || r.Reviewer == "" {
		r.Reason = "artifact lacks exact sha/reviewer identity; no resident mutation attempted"
		return r, nil
	}
	if censusErr != nil {
		r.Disposition, r.Reason = "blocked", "agent census unavailable: "+censusErr.Error()
		return r, censusErr
	}
	var matches []herdr.AgentEntry
	for _, agent := range agents {
		if strings.TrimSpace(agent.Name) == r.Reviewer {
			matches = append(matches, agent)
		}
	}
	if len(matches) == 0 {
		r.Disposition, r.Reason = "already_absent", "no live reviewer with exact artifact identity"
		return r, nil
	}
	if len(matches) != 1 {
		r.Disposition, r.Reason = "blocked", fmt.Sprintf("exact reviewer identity is ambiguous (%d matches)", len(matches))
		return r, errors.New(r.Reason)
	}
	agent := matches[0]
	r.TabID, r.PaneID = agent.TabID, agent.PaneID
	if agent.Status != "idle" && agent.Status != "done" {
		r.Reason = fmt.Sprintf("reviewer status %q is not settled", agent.Status)
		return r, nil
	}
	if agent.Focused == nil || *agent.Focused {
		r.Reason = "reviewer focus is not explicitly false"
		return r, nil
	}
	if strings.TrimSpace(agent.Cwd) == "" || strings.TrimSpace(agent.TabID) == "" ||
		strings.TrimSpace(agent.PaneID) == "" || strings.TrimSpace(agent.Workspace) == "" ||
		strings.TrimSpace(agent.TerminalID) == "" || strings.TrimSpace(agent.Session.Value) == "" {
		r.Disposition, r.Reason = "blocked", "reviewer live identity is incomplete"
		return r, errors.New(r.Reason)
	}
	head, err := transportedReviewerHead(agent.Cwd)
	if err != nil || !strings.EqualFold(strings.TrimSpace(head), r.SHA) {
		r.Disposition, r.Reason = "blocked", fmt.Sprintf("candidate HEAD mismatch: got=%q want=%q error=%v", strings.TrimSpace(head), r.SHA, err)
		return r, errors.New(r.Reason)
	}
	if dryRun {
		r.Disposition, r.Reason = "would_reap", "remote verdict ref present and all resident identity gates passed"
		return r, nil
	}
	if err := transportedReviewerClose(agent); err != nil {
		r.Disposition, r.Reason = "blocked", "exact tab/process-tree cleanup: "+err.Error()
		return r, err
	}
	r.Disposition, r.Reason = "reaped", "remote verdict ref present; exact settled reviewer and captured process tree retired"
	return r, nil
}

func appendReviewReapReceipt(root string, receipt reviewReapReceipt) error {
	path := filepath.Join(root, ".herd", "review", "reap.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
