package mergeadmit

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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
