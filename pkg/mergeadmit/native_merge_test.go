package mergeadmit

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

type nativeMergeFixture struct {
	t                          *testing.T
	root, remote, base, head   string
	merger                     *NativeMerge
	pushes                     int
	race, rewind, loseResponse bool
}

func newNativeMergeFixture(t *testing.T) *nativeMergeFixture {
	t.Helper()
	root := gitRepo(t)
	remote := t.TempDir()
	run(t, remote, "git", "init", "--bare", "-q", "-b", "main")
	run(t, root, "git", "remote", "set-url", "origin", remote)
	commit(t, root, "seed.txt", "seed\n", "seed")
	base := commit(t, root, "base.txt", "base\n", "base")
	run(t, root, "git", "push", "-q", "origin", "main")
	run(t, root, "git", "checkout", "-q", "-b", "integration/merge-fixture")
	head := commit(t, root, "candidate.txt", "candidate\n", "candidate")
	ledger := newLedger(t, root)
	launch(t, ledger, head, "review-fixture", "anthropic", "builder-session-1")
	verdict(t, ledger, head, "review-fixture", reviewledger.Verdict("PASS"))
	gate := okGate(t, ledger, base, head)
	gate.RepoDir = root
	publisher := &harvest.IntegrationPRPublisher{RepoRoot: root, Binding: harvest.IntegrationPRBinding{Repository: "github.com/Kampe/Herdforge", Candidate: head, Head: head, Branch: "integration/merge-fixture", Base: "main"}}
	f := &nativeMergeFixture{t: t, root: root, remote: remote, base: base, head: head}
	f.merger = NewNativeMerge(publisher, gate, okRequest(base, head))
	f.merger.command = f.command
	f.merger.observePR = f.observe
	return f
}
func (f *nativeMergeFixture) observe(context.Context) (*harvest.IntegrationPR, error) {
	cross := false
	pr := &harvest.IntegrationPR{Number: 739, State: "OPEN", URL: "https://github.com/Kampe/Herdforge/pull/739", Head: f.head, Branch: "integration/merge-fixture", Base: "main", CrossRepo: &cross}
	if revParse(f.t, f.remote, "refs/heads/main") == f.head {
		pr.State = "MERGED"
		pr.MergeCommit.OID = f.head
		pr.MergedAt = "2026-09-07T12:00:00Z"
	}
	return pr, nil
}
func (f *nativeMergeFixture) command(ctx context.Context, name string, args []string) ([]byte, error) {
	f.t.Helper()
	if name != "git" {
		f.t.Fatalf("unexpected command %s", name)
	}
	if args[0] == "push" {
		f.pushes++
		if f.race {
			tree := revParse(f.t, f.root, f.base+"^{tree}")
			other := strings.TrimSpace(runOut(f.t, f.root, "git", "commit-tree", tree, "-p", f.base, "-m", "competing integration"))
			if f.rewind {
				other = revParse(f.t, f.root, f.base+"^")
			}
			run(f.t, f.root, "git", "push", "-q", "origin", other+":refs/heads/competitor")
			run(f.t, f.remote, "git", "update-ref", "refs/heads/main", other, f.base)
		}
	}
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = f.root
	out, err := cmd.CombinedOutput()
	if err == nil && args[0] == "push" && f.loseResponse {
		return nil, errors.New("lost response after successful fixture publication")
	}
	return out, err
}
func TestFAC601NativeMergeUsesCompiledAdmissionAndExactCAS(t *testing.T) {
	f := newNativeMergeFixture(t)
	pr, err := f.merger.Publish(context.Background())
	if err != nil || pr == nil || pr.State != "MERGED" {
		t.Fatalf("merge failed: %+v %v", pr, err)
	}
	if f.pushes != 1 || revParse(t, f.remote, "refs/heads/main") != f.head {
		t.Fatal("exact candidate was not published")
	}
}
func TestFAC601NativeMergeRejectsAdmissionFailure(t *testing.T) {
	f := newNativeMergeFixture(t)
	f.merger.gate.Ledger = nil
	if _, err := f.merger.Publish(context.Background()); err == nil {
		t.Fatal("missing admission was accepted")
	}
	if f.pushes != 0 || revParse(t, f.remote, "refs/heads/main") != f.base {
		t.Fatal("refused candidate published")
	}
}
func TestFAC601NativeMergeRejectsConcurrentBaseAdvance(t *testing.T) {
	f := newNativeMergeFixture(t)
	f.race = true
	if _, err := f.merger.Publish(context.Background()); err == nil {
		t.Fatal("stale admitted base was overwritten")
	}
	if f.pushes != 1 {
		t.Fatal("test did not reach server-side race")
	}
	if revParse(t, f.remote, "refs/heads/main") == f.head {
		t.Fatal("competing base was overwritten")
	}
}
func TestFAC601NativeMergeLostResponseIsObservedWithoutRepublish(t *testing.T) {
	f := newNativeMergeFixture(t)
	f.loseResponse = true
	if _, err := f.merger.Publish(context.Background()); err == nil {
		t.Fatal("uncertain response reported success")
	}
	pr, err := f.merger.observe(context.Background())
	if err != nil || pr == nil || pr.State != "MERGED" {
		t.Fatalf("completed effect not observed: %+v %v", pr, err)
	}
	if f.pushes != 1 {
		t.Fatal("observation repeated publication")
	}
}

func TestFAC601NativeMergeRejectsConcurrentBaseRewind(t *testing.T) {
	f := newNativeMergeFixture(t)
	f.race = true
	f.rewind = true
	if _, err := f.merger.Publish(context.Background()); err == nil {
		t.Fatal("changed expected base was accepted because publication was still a fast-forward")
	}
	if f.pushes != 1 {
		t.Fatal("fixture did not reach remote transaction")
	}
	if revParse(t, f.remote, "refs/heads/main") != revParse(t, f.root, f.base+"^") {
		t.Fatal("rewound base was overwritten")
	}
}

func TestFAC601NativeMergeRetainsFreshDecisionBeforePublication(t *testing.T) {
	f := newNativeMergeFixture(t)
	called := false
	_, err := f.merger.PublishRecorded(context.Background(), func(req Request, d Decision) error {
		called = true
		if f.pushes != 0 || !d.Admitted || req.CandidateSHA != f.head || d.CandidateSHA != f.head || d.BaseSHA != f.base || d.VerificationDigest == "" {
			t.Fatal("recorder did not receive the fresh pre-publication decision")
		}
		return errors.New("fixture persistence failed")
	})
	if err == nil || !called || f.pushes != 0 || revParse(t, f.remote, "refs/heads/main") != f.base {
		t.Fatalf("failed persistence did not fence publication: called=%t pushes=%d error=%v", called, f.pushes, err)
	}
}
