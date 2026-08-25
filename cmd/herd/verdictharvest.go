package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
// gitShowToFile materialises one path from a ref without staging it.
func gitShowToFile(root, ref, path string) (string, error) {
	cmd := exec.Command("git", "-C", root, "show", ref+":"+path)
	blob, err := cmd.Output()
	if err != nil {
		return strings.TrimSpace(string(blob)), err
	}
	full := filepath.Join(root, path)
	if mkErr := os.MkdirAll(filepath.Dir(full), 0o755); mkErr != nil {
		return "", mkErr
	}
	// Never clobber: the caller has already established this path is absent, and
	// an exclusive create keeps a concurrent harvest from racing it.
	fh, openErr := os.OpenFile(full, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if openErr != nil {
		if os.IsExist(openErr) {
			return "", nil
		}
		return "", openErr
	}
	defer fh.Close()
	if _, wErr := fh.Write(blob); wErr != nil {
		return "", wErr
	}
	return "", nil
}

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
			// FAC-633: write the file WITHOUT touching the index.
			//
			// `git checkout <ref> -- <path>` stages what it extracts. Harvesting
			// therefore left dozens of verdict artifacts staged in whoever ran it,
			// and chainseer's em-dash guard then blocked three unrelated commits of
			// mine on prose inside those artifacts. A read-only collection step must
			// not mutate the caller's index.
			//
			// `git show <ref>:<path>` reads the blob and writes it directly, which is
			// what harvesting actually means.
			if out, coErr := gitShowToFile(root, ref, path); coErr != nil {
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
