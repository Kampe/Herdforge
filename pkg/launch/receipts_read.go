package launch

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultReceiptPath is the launch-receipt log DefaultSink writes.
//
// FAC-637: exported so provenance resolution reads the SAME file dispatch
// writes. Re-deriving the path in cmd/ would be two copies of one rule, and two
// copies drift while only one gets fixed -- the defect FAC-613 removed here.
func DefaultReceiptPath() string {
	if p := strings.TrimSpace(os.Getenv("HERD_LAUNCH_RECEIPTS")); p != "" {
		return p
	}
	return PathFor("")
}

// PathFor returns the receipt path rooted at root without applying an
// environment override. Callers that have already established the canonical
// project root use this to compare an override against that project.
func PathFor(root string) string {
	if strings.TrimSpace(root) == "" {
		return filepath.Join(".herd", "launch-receipts.jsonl")
	}
	return filepath.Join(root, ".herd", "launch-receipts.jsonl")
}

// ReceiptPathFor anchors the receipt log on an explicit repository root.
//
// FAC-646: DefaultReceiptPath is cwd-relative, which is the third instance of
// the FAC-643 class in this tree (after the review ledger and the pulse sweep).
// Callers that already KNOW the repository root were silently reading whichever
// receipt log happened to sit under the process's cwd -- measured from the
// Herdforge checkout it returned that repo's 12 receipts instead of the target
// project's. A caller holding the root must be able to say so.
func ReceiptPathFor(root string) string {
	if p := strings.TrimSpace(os.Getenv("HERD_LAUNCH_RECEIPTS")); p != "" {
		return p
	}
	return PathFor(root)
}

// ReadReceipts returns every parseable receipt, oldest first. A missing log is
// not an error: a fleet that has never recorded a launch has no receipts, which
// is different from a log that cannot be read.
func ReadReceipts(path string) ([]Receipt, error) {
	fh, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer fh.Close()

	var out []Receipt
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 0, 256*1024), 8*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r Receipt
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			// Skip a malformed row rather than abandoning the log: one bad append
			// must not make every earlier launch unprovable.
			continue
		}
		out = append(out, r)
	}
	if err := sc.Err(); err != nil {
		return out, err
	}
	return out, nil
}

// BuilderFamilyForBranch resolves recorded builder provenance for a branch.
//
// FAC-620: review intake previously took builder family from a HEADER a
// reviewer typed. That is an assertion, not provenance -- and after FAC-615's
// provider fallthrough it is an assertion about a route the reviewer did not
// observe. A lane configured for codex that ran claude would be attested as
// whatever the reviewer believed.
//
// A launch receipt saying "lane X ran claude on branch Y", plus a commit on
// branch Y, is traceable. This is the read side of that join.
//
// The LAST accepted receipt for the branch wins: a branch may be relaunched,
// and the most recent launch is the one that produced the current tip.
// Receipts without a family are skipped rather than treated as empty evidence,
// because a receipt that could not derive a family was refused at write time
// and any such row predates that rule.
func BuilderFamilyForBranch(path, branch string) (string, bool) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", false
	}
	receipts, err := ReadReceipts(path)
	if err != nil {
		return "", false
	}
	family := ""
	for _, r := range receipts {
		if !r.Accepted {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(r.Branch), branch) {
			continue
		}
		if f := strings.TrimSpace(r.BuilderFamily); f != "" {
			family = f
		}
	}
	return family, family != ""
}

// BuilderFamilyReachingSHA resolves builder provenance from a receipt whose
// branch PROVABLY contains the reviewed commit.
//
// FAC-620: matching branch text alone is not evidence. Branches are reused,
// relaunched and rebased, so a receipt naming a branch says nothing about
// whether that branch still reaches the SHA under review. Attributing a family
// on the name alone yields confident WRONG provenance -- worse than none,
// because independence is computed against it and it looks authoritative.
//
// reaches is injected so the caller supplies real git ancestry and tests need
// no repository. A receipt whose branch does not reach the SHA is skipped, not
// guessed at.
//
// Reachability alone is necessary but NOT sufficient, and the third review of
// this card is what found the hole: standing lanes are relaunched on the SAME
// branch as normal fleet operation, so
//
//	T0 launch A (anthropic) -> T1 A commits abc123 -> T2 relaunch B (xai)
//
// leaves BOTH receipts reaching abc123, and last-in-file-order handed the
// commit to B -- a launch that had not started when abc123 was written. So the
// receipt must also PREDATE the commit, and among those the latest wins: that
// is the launch that was running when the commit landed.
//
// commitTime is the reviewed commit's author time. A zero commitTime, or a
// receipt with no timestamp, cannot be ordered and so is not provenance --
// unknown must never read as proven.
func BuilderFamilyReachingSHA(path, sha string, commitTime time.Time, reaches func(branch string) bool) (string, bool) {
	if strings.TrimSpace(sha) == "" || reaches == nil || commitTime.IsZero() {
		return "", false
	}
	receipts, err := ReadReceipts(path)
	if err != nil {
		return "", false
	}
	family := ""
	var best time.Time
	for _, r := range receipts {
		if !r.Accepted {
			continue
		}
		branch := strings.TrimSpace(r.Branch)
		fam := strings.TrimSpace(r.BuilderFamily)
		if branch == "" || fam == "" || r.CreatedAt.IsZero() {
			continue
		}
		if r.CreatedAt.After(commitTime) {
			continue
		}
		if !best.IsZero() && r.CreatedAt.Before(best) {
			continue
		}
		if !reaches(branch) {
			continue
		}
		family, best = fam, r.CreatedAt
	}
	return family, family != ""
}
