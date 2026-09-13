package gitroot

import (
	"errors"
	"fmt"
	"strings"
)

// FAC-831: one definition of the reviewed-delta replay, shared by the producer
// that seals a receipt and the consumer that validates one.
//
// Both sides ask the same question — does this integration commit hold the
// result of applying the reviewed base..candidate delta? — and both must ask it
// the same way. A consumer that instead compared paths, or blobs, refused
// honest landings: a rename with an edit in one reviewed commit, and main and
// the pull request editing different hunks of one file, both look like
// "changed" to a path or blob comparison and like the expected tree to a
// replay. Comparing the WHOLE resulting tree is what distinguishes them, and it
// still refuses an ours merge or a substituted-bytes merge, because those
// produce a different tree.
//
// This lives in the leaf so neither side imports the other, and it starts NO
// subprocess: the caller supplies the runner, so the caller's own command,
// output and deadline budget governs the work and nothing here can escape it.

// ReplayRunner executes one git invocation and returns its trimmed stdout.
//
// The caller owns the process: its bounds, its deadline, its cancellation and
// its child reaping. An error it returns is passed back unchanged, so a
// cancellation or a budget refusal stays recognisable to errors.Is instead of
// being flattened into a content answer.
type ReplayRunner func(args ...string) (string, error)

var (
	// ErrNilReplayRunner means no runner was supplied. The leaf will not fall
	// back to starting its own process: that would escape the caller's budget,
	// which is the entire reason the runner is a parameter.
	ErrNilReplayRunner = errors.New("gitroot replay: a runner is required")
	// ErrEmptyReplayIdentity means one of the three identities is missing. A
	// blank identity would make git resolve something else, so it is refused
	// before any command runs.
	ErrEmptyReplayIdentity = errors.New("gitroot replay: base, parent and candidate are all required")
)

// ReplayArgs is the one definition of the replay argv for the RECEIPT PROOF
// contract: the producer in pkg/mergeadmit and the consumer in pkg/sync both
// build it here and cannot drift into two spellings of the same command.
//
// Scope stated honestly: pkg/review has its own merge-tree invocations for a
// different purpose. They are outside this contract and outside this lane's
// ownership, so they are untouched rather than quietly folded in.
func ReplayArgs(base, parent, candidate string) []string {
	return []string{"merge-tree", MergeTreeWriteFlag, "--merge-base", base, parent, candidate}
}

// ReplayReviewedTree applies base..candidate onto parent, using base as the
// EXACT merge base, and returns the resulting tree.
//
// The caller compares that tree against the integration commit's own tree. It
// is the same predicate the producer already used before this moved down here;
// only its home changed.
func ReplayReviewedTree(base, parent, candidate string, run ReplayRunner) (string, error) {
	if run == nil {
		return "", ErrNilReplayRunner
	}
	base, parent, candidate = strings.TrimSpace(base), strings.TrimSpace(parent), strings.TrimSpace(candidate)
	if base == "" || parent == "" || candidate == "" {
		return "", fmt.Errorf("%w (base=%q parent=%q candidate=%q)",
			ErrEmptyReplayIdentity, base, parent, candidate)
	}
	// Unwrapped on purpose: the caller's cancellation and budget errors must
	// reach the caller as themselves.
	return run(ReplayArgs(base, parent, candidate)...)
}
