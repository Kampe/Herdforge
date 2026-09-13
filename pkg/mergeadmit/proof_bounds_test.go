package mergeadmit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// boundedProofRepo builds a real range with several commits, so the bounds
// below are reached by ACTUAL proof work rather than by a contrived call.
func boundedProofRepo(t *testing.T, extra int) (dir, base, candidate string) {
	t.Helper()
	dir = gitRepo(t)
	base = commit(t, dir, "a.txt", "base\n", "base")
	run(t, dir, "git", "checkout", "-q", "-b", "work")
	for i := 0; i < extra; i++ {
		candidate = commit(t, dir, "b.txt", strings.Repeat("x", i+1)+"\n", "candidate step")
	}
	return dir, base, candidate
}

// The command budget must be reached by real proof work and refuse, rather than
// being a number nothing ever consults.
func TestProofCommandBudgetRefusesInsteadOfRunningUnbounded(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 4)
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{MaxCommands: 2})
	defer cancel()

	_, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
		BaseSHA: base, CandidateSHA: candidate, LandedSHA: candidate,
	})
	if err == nil {
		t.Fatal("a two-command allowance proved a multi-commit range; the command budget is not consulted by real work")
	}
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands", err)
	}
}

// The range budget must refuse a range larger than the allowance, BEFORE the
// replay cap downstream could apply.
func TestProofRangeBudgetRefusesAnOversizedRange(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 4)
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{MaxRangeCommits: 2})
	defer cancel()

	_, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
		BaseSHA: base, CandidateSHA: candidate, LandedSHA: candidate,
	})
	if err == nil {
		t.Fatal("a two-commit range allowance materialised a four-commit range")
	}
	if !errors.Is(err, ErrProofBudgetRange) {
		t.Fatalf("err = %v, want ErrProofBudgetRange", err)
	}
}

// The output budget must refuse a command whose stdout exceeds the allowance,
// at the boundary, rather than buffering it whole.
func TestProofOutputBudgetRefusesOversizedCommandOutput(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 2)
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{MaxOutputBytes: 1})
	defer cancel()

	_, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
		BaseSHA: base, CandidateSHA: candidate, LandedSHA: candidate,
	})
	if err == nil {
		t.Fatal("a one-byte output allowance accepted real git output")
	}
	if !errors.Is(err, ErrProofBudgetOutput) {
		t.Fatalf("err = %v, want ErrProofBudgetOutput", err)
	}
}

// The deadline must be SHARED by the whole proof. An already-expired budget
// must refuse before doing work, and the refusal must be the context error, not
// a content verdict.
func TestProofDeadlineIsSharedAndRefusesExpired(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 2)
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{Deadline: time.Nanosecond})
	defer cancel()
	time.Sleep(2 * time.Millisecond)

	_, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
		BaseSHA: base, CandidateSHA: candidate, LandedSHA: candidate,
	})
	if err == nil {
		t.Fatal("an expired proof deadline still produced a proof")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// POSITIVE control: with the default allowance the same real work SUCCEEDS, so
// the refusals above cannot be satisfied by a producer that refuses everything.
func TestProofDefaultBudgetStillProvesAnOrdinaryLanding(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 2)
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{})
	defer cancel()

	proof, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
		BaseSHA: base, CandidateSHA: candidate, LandedSHA: candidate,
	})
	if err != nil {
		t.Fatalf("the default allowance refused an ordinary landing: %v", err)
	}
	if proof == nil || proof.MergeSHA == "" {
		t.Fatalf("default-budget proof is empty: %+v", proof)
	}
}

// Every exported entry installs an allowance, so no production path runs
// unbounded. An unbudgeted context gains one; an already-budgeted context keeps
// the one it has, so a nested call cannot award itself a fresh allowance.
func TestEnsureProofBudgetInstallsOnceAndNeverReplaces(t *testing.T) {
	plain, cancel := ensureProofBudget(context.Background())
	defer cancel()
	first := ledgerFrom(plain)
	if first == nil {
		t.Fatal("an unbudgeted context was left without an allowance")
	}
	if _, ok := plain.Deadline(); !ok {
		t.Fatal("the installed allowance carries no deadline")
	}

	again, cancel2 := ensureProofBudget(plain)
	defer cancel2()
	if ledgerFrom(again) != first {
		t.Fatal("a nested call replaced the allowance; spend would reset mid-proof")
	}
}

// The ledger itself: spending is cumulative and refuses past the allowance.
func TestProofLedgerRefusesPastItsAllowance(t *testing.T) {
	l := &proofLedger{limits: ProofBudget{MaxCommands: 2}.withDefaults()}
	if err := l.spendCommand([]string{"a"}); err != nil {
		t.Fatalf("first command refused: %v", err)
	}
	if err := l.spendCommand([]string{"b"}); err != nil {
		t.Fatalf("second command refused: %v", err)
	}
	err := l.spendCommand([]string{"c"})
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("third command err = %v, want ErrProofBudgetCommands", err)
	}
	if !strings.Contains(err.Error(), "2 commands already run") {
		t.Fatalf("refusal does not say what was spent: %v", err)
	}
}

// boundedBuffer refuses at the boundary instead of returning a short answer.
func TestBoundedBufferRefusesRatherThanTruncating(t *testing.T) {
	w := &boundedBuffer{limit: 4}
	if n, err := w.Write([]byte("abcd")); n != 4 || err != nil {
		t.Fatalf("write within the limit = (%d, %v)", n, err)
	}
	if _, err := w.Write([]byte("e")); !errors.Is(err, ErrProofBudgetOutput) {
		t.Fatalf("overflow err = %v, want ErrProofBudgetOutput", err)
	}
	if !w.overflowed() {
		t.Fatal("overflow was not recorded")
	}
	if got := string(w.Bytes()); got != "abcd" {
		t.Fatalf("buffer = %q; a refused write must not extend it", got)
	}
}

// REGRESSION (CI 34741746509): a budget refusal reached the caller as
// "base revision does not resolve", because resolveCommit dropped the cause.
// Both reported failures were that flattening, not a missing bound.
func TestProofBudgetSurvivesRevisionResolution(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 2)
	for _, tc := range []struct {
		name   string
		budget ProofBudget
		want   error
	}{
		{"commands exhausted at the first resolve", ProofBudget{MaxCommands: 0}, ErrProofBudgetCommands},
		{"output exhausted at the first resolve", ProofBudget{MaxOutputBytes: 1}, ErrProofBudgetOutput},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := withProofBudget(context.Background(), tc.budget)
			defer cancel()
			_, err := ProveEquivalentLandedContext(ctx, dir, ProofRequest{
				BaseSHA: base, CandidateSHA: candidate, LandedSHA: candidate,
			})
			if err == nil {
				t.Fatal("an exhausted allowance produced a proof")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want it to stay recognisable as %v; a flattened budget error reads as an ordinary resolution failure", err, tc.want)
			}
		})
	}
}

// A budget refusal inside the replay must ABORT the search, never be reported
// as "this commit does not preserve the content". That would silently skip a
// candidate and can end in a wrong refusal or a wrong selection.
func TestContentPreservedAtAbortsOnBudgetRatherThanAnsweringFalse(t *testing.T) {
	dir, base, carrier, candidate, mergeCommit := prShapedLanding(t)
	_ = carrier
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{MaxCommands: 0})
	defer cancel()

	ok, err := contentPreservedAt(ctx, dir, base, candidate, mergeCommit)
	if err == nil {
		t.Fatalf("an exhausted allowance answered the content question: preserved=%v", ok)
	}
	if ok {
		t.Fatal("an aborted run reported the content preserved")
	}
	if !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want ErrProofBudgetCommands", err)
	}
}

// The exported replay wrappers must carry the same finite allowance. Before
// this, ReplayTreeContext reached the primitive without installing one.
func TestReplayTreeContextCarriesAnAllowance(t *testing.T) {
	dir, base, candidate := boundedProofRepo(t, 1)
	ctx, cancel := withProofBudget(context.Background(), ProofBudget{MaxCommands: 0})
	defer cancel()
	if _, err := ReplayTreeContext(ctx, dir, base, base, candidate); !errors.Is(err, ErrProofBudgetCommands) {
		t.Fatalf("err = %v, want the exported wrapper to honour the allowance", err)
	}
}

// The leaf refuses before running anything, and never starts a process itself.
func TestGitrootReplayRefusesNilRunnerAndEmptyIdentities(t *testing.T) {
	if _, err := gitroot.ReplayReviewedTree("a", "b", "c", nil); !errors.Is(err, gitroot.ErrNilReplayRunner) {
		t.Fatalf("nil runner err = %v", err)
	}
	called := false
	run := func(...string) (string, error) { called = true; return "", nil }
	for _, tc := range [][3]string{{"", "b", "c"}, {"a", "", "c"}, {"a", "b", ""}, {" ", "b", "c"}} {
		if _, err := gitroot.ReplayReviewedTree(tc[0], tc[1], tc[2], run); !errors.Is(err, gitroot.ErrEmptyReplayIdentity) {
			t.Fatalf("identities %v err = %v", tc, err)
		}
	}
	if called {
		t.Fatal("the leaf ran a command for a refused input")
	}
}

// The leaf passes the runner's error back unchanged, so a caller's cancellation
// or budget refusal stays recognisable rather than becoming a content answer.
func TestGitrootReplayPassesRunnerErrorsThroughUnchanged(t *testing.T) {
	sentinel := errors.New("caller budget refused")
	_, err := gitroot.ReplayReviewedTree("a", "b", "c", func(...string) (string, error) { return "", sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want the runner's own error", err)
	}
}

// One argv, defined once, and it is the one the producer uses.
func TestGitrootReplayArgsIsTheSoleDefinition(t *testing.T) {
	got := gitroot.ReplayArgs("B", "P", "C")
	want := []string{"merge-tree", gitroot.MergeTreeWriteFlag, "--merge-base", "B", "P", "C"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv = %v, want %v", got, want)
		}
	}
}
