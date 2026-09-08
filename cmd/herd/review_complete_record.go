package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/classify"
	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

func runReviewCompleteRecord() error {
	args := os.Args[2:]
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("usage: herd review-complete-record REF --candidate SHA --reviewer NAME --host HOST --artifact FILE")
	}
	task := args[0]
	fs := flag.NewFlagSet("review-complete-record", flag.ContinueOnError)
	sha := fs.String("candidate", "", "exact admitted candidate")
	reviewer := fs.String("reviewer", "", "original admitted reviewer")
	host := fs.String("host", "", "exact admitted reviewer host; empty selects only unhosted records")
	artifact := fs.String("artifact", "", "retained admitted artifact")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	decoded, err := hex.DecodeString(*sha)
	if err != nil || len(decoded) != 20 || *reviewer == "" || *artifact == "" || fs.NArg() != 0 {
		return fmt.Errorf("exact SHA, reviewer and retained artifact required")
	}
	root, _, err := gitroot.ProjectRoot(context.Background(), ".")
	if err != nil {
		return err
	}
	l, err := reviewledger.NewReadOnlyReviewLedger(root, reviewLedgerPath())
	if err != nil {
		return err
	}
	return l.CompleteAdmissionRecord(task, *sha, *reviewer, *host, func(v reviewledger.LedgerRow) (reviewledger.RecordCompletion, error) {
		var out reviewledger.RecordCompletion
		body, err := os.ReadFile(*artifact)
		if err != nil {
			return out, err
		}
		sum := sha256.Sum256(body)
		if hex.EncodeToString(sum[:]) != v.ArtifactDigest {
			return out, fmt.Errorf("retained artifact digest differs from current admitted verdict")
		}
		a := reviewingest.Parse(string(body))
		if a.SHA != *sha || a.ReadHead != *sha || a.TaskRef != task || a.Reviewer != *reviewer || a.Verdict != "PASS" || a.ReviewerFamily != v.ReviewerFamily {
			return out, fmt.Errorf("retained artifact identity differs from admitted verdict")
		}
		base, err := hex.DecodeString(a.ReadBase)
		if err != nil || len(base) != 20 || !commitIsAncestor(root, a.ReadBase, a.SHA) {
			return out, fmt.Errorf("retained reviewed base must be an exact ancestor")
		}
		proven, err := reviewingest.ReconcileBuilderFamilyForSHA(&a, launch.ReceiptPathFor(root), a.SHA, commitCreationTime(a.SHA), func(branch, sha string) bool { return commitIsAncestor(root, sha, branch) })
		if err != nil {
			return out, err
		}
		if !proven || a.BuilderFamily != v.BuilderFamily {
			return out, fmt.Errorf("missing or conflicting native reaching launch receipt")
		}
		if a.VerificationDigest() != v.VerificationDigest {
			return out, fmt.Errorf("retained verification differs from admitted verdict")
		}
		paths, _, _, err := diffStat(a.ReadBase, a.SHA)
		if err != nil {
			return out, err
		}
		if len(paths) == 0 {
			return out, fmt.Errorf("empty reviewed candidate")
		}
		out.Branch = a.Branch
		out.Tier = string(classify.Classify(classify.Input{CandidateSHA: a.SHA, Paths: paths}).Tier)
		return out, nil
	})
}
