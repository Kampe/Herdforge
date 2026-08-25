package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runVerdictHarvest pulls verdict artifacts pushed by other hosts into the local
// inbox, so review-ingest --sweep can admit them.
//
// FAC-621: verdict-push (FAC-619) gave reviewers a working way to send a verdict,
// and refs immediately began accumulating on origin -- six of them unharvested
// within one beat, because collecting them was a multi-step git loop somebody had
// to remember: fetch a refspec glob, enumerate refs, ls-tree each one, checkout
// only the artifacts not already present.
//
// That is the same shape as the inbox before --sweep existed. A recovery step
// that has to be remembered is not a recovery step, and instructing the fleet to
// remember it did not work the last two times. So it is a command.
func runVerdictHarvest() error {
	fs := flag.NewFlagSet("verdict-harvest", flag.ContinueOnError)
	remote := fs.String("remote", "origin", "Git remote carrying verdicts/* refs")
	dryRun := fs.Bool("dry-run", false, "Report what would be harvested without writing")
	if err := fs.Parse(os.Args[2:]); err != nil {
		return err
	}
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	git := func(args ...string) (string, error) {
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}

	if out, err := git("fetch", "-q", *remote,
		"+refs/heads/verdicts/*:refs/remotes/"+*remote+"/verdicts/*"); err != nil {
		// Fail closed: an unreachable remote is not "no verdicts waiting".
		return fmt.Errorf("fetch verdict refs from %s: %s", *remote, out)
	}

	refs, err := git("for-each-ref", "--format=%(refname:short)",
		"refs/remotes/"+*remote+"/verdicts")
	if err != nil {
		return fmt.Errorf("list verdict refs: %s", refs)
	}
	harvested, skipped := 0, 0
	for _, ref := range strings.Fields(refs) {
		listing, lsErr := git("ls-tree", "-r", "--name-only", ref)
		if lsErr != nil {
			fmt.Fprintf(os.Stderr, "herd verdict-harvest: skipping %s: %s\n", ref, listing)
			continue
		}
		for _, path := range strings.Split(listing, "\n") {
			path = strings.TrimSpace(path)
			if !strings.HasPrefix(path, ".herd/review/inbox/") || !strings.HasSuffix(path, ".md") {
				continue
			}
			// Never overwrite a local artifact. The local copy may already be
			// ingested, and clobbering it would resurrect a settled verdict.
			if _, statErr := os.Stat(path); statErr == nil {
				skipped++
				continue
			}
			if *dryRun {
				fmt.Printf("WOULD_HARVEST %s from %s\n", path, ref)
				harvested++
				continue
			}
			if out, coErr := git("checkout", ref, "--", path); coErr != nil {
				fmt.Fprintf(os.Stderr, "herd verdict-harvest: could not extract %s: %s\n", path, out)
				continue
			}
			fmt.Printf("HARVESTED %s from %s\n", path, ref)
			harvested++
		}
	}
	fmt.Printf("herd verdict-harvest: harvested=%d already_present=%d refs=%d\n",
		harvested, skipped, len(strings.Fields(refs)))
	if harvested > 0 && !*dryRun {
		fmt.Println("herd verdict-harvest: run `herd review-ingest --sweep` to admit them")
	}
	return nil
}
