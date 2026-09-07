package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

type reviewHostIngestArgs struct {
	Candidate      string
	Reviewer       string
	Receipt        string
	Artifact       string
	ProductionBase string
}

func parseReviewHostIngestArgs(args []string) (reviewHostIngestArgs, error) {
	var out reviewHostIngestArgs
	usage := fmt.Errorf("usage: herd review-ledger host-ingest --candidate SHA --reviewer NAME --receipt FILE [--artifact FILE] [--base SHA]")
	for i := 0; i < len(args); i++ {
		a := args[i]
		need := func() (string, error) {
			if i+1 >= len(args) {
				return "", usage
			}
			i++
			return strings.TrimSpace(args[i]), nil
		}
		switch {
		case a == "--host" || strings.HasPrefix(a, "--host=") || a == "--family" || strings.HasPrefix(a, "--family="):
			return out, fmt.Errorf("%s is not authentication; host and family must come from the launch receipt chain", strings.SplitN(a, "=", 2)[0])
		case a == "--candidate" || strings.HasPrefix(a, "--candidate="):
			if strings.HasPrefix(a, "--candidate=") {
				out.Candidate = strings.TrimSpace(strings.TrimPrefix(a, "--candidate="))
				continue
			}
			v, err := need()
			if err != nil {
				return out, err
			}
			out.Candidate = v
		case a == "--reviewer" || strings.HasPrefix(a, "--reviewer="):
			if strings.HasPrefix(a, "--reviewer=") {
				out.Reviewer = strings.TrimSpace(strings.TrimPrefix(a, "--reviewer="))
				continue
			}
			v, err := need()
			if err != nil {
				return out, err
			}
			out.Reviewer = v
		case a == "--receipt" || strings.HasPrefix(a, "--receipt="):
			if strings.HasPrefix(a, "--receipt=") {
				out.Receipt = strings.TrimSpace(strings.TrimPrefix(a, "--receipt="))
				continue
			}
			v, err := need()
			if err != nil {
				return out, err
			}
			out.Receipt = v
		case a == "--artifact" || strings.HasPrefix(a, "--artifact="):
			if strings.HasPrefix(a, "--artifact=") {
				out.Artifact = strings.TrimSpace(strings.TrimPrefix(a, "--artifact="))
				continue
			}
			v, err := need()
			if err != nil {
				return out, err
			}
			out.Artifact = v
		case a == "--base" || strings.HasPrefix(a, "--base="):
			if strings.HasPrefix(a, "--base=") {
				out.ProductionBase = strings.TrimSpace(strings.TrimPrefix(a, "--base="))
				continue
			}
			v, err := need()
			if err != nil {
				return out, err
			}
			out.ProductionBase = v
		case a == "--sweep" || a == "--corpus" || strings.HasPrefix(a, "--sweep=") || strings.HasPrefix(a, "--corpus="):
			return out, fmt.Errorf("corpus mode is refused; host-ingest is explicit SHA/reviewer/receipt only")
		case strings.HasPrefix(a, "-"):
			return out, fmt.Errorf("unknown flag %s", a)
		default:
			return out, usage
		}
	}
	if out.Candidate == "" || out.Reviewer == "" || out.Receipt == "" {
		return out, usage
	}
	return out, nil
}

func launchProvenanceFromReceipt(receipt launch.Receipt) (reviewledger.LaunchProvenance, error) {
	host := reviewledger.HostFromLaunchProof(receipt.ProcessIdentity, receipt.HerdrSession, receipt.PaneID, receipt.CWD, receipt.Worktree)
	if host == "" {
		return reviewledger.LaunchProvenance{}, fmt.Errorf("launch receipt does not authenticate a host or session")
	}
	return reviewledger.LaunchProvenance{
		CandidateSHA:  strings.TrimSpace(receipt.CandidateSHA),
		Host:          host,
		Session:       firstNonEmptyCLI(receipt.ProcessIdentity, receipt.HerdrSession, receipt.PaneID),
		BuilderFamily: strings.TrimSpace(receipt.BuilderFamily),
		Branch:        strings.TrimSpace(receipt.Branch),
		CreatedAt:     receipt.CreatedAt,
		Accepted:      receipt.Accepted,
	}, nil
}

func firstNonEmptyCLI(values ...string) string {
	for _, v := range values {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}

func commitTimeOf(root, sha string) time.Time {
	cmd := exec.Command("git", "-C", root, "show", "-s", "--format=%cI", sha)
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(string(out)))
	if err != nil {
		return time.Time{}
	}
	return parsed
}

func branchReaches(root, branch, sha string) bool {
	cmd := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", sha, branch)
	return cmd.Run() == nil
}

func runReviewHostIngest(args []string) error {
	parsed, err := parseReviewHostIngestArgs(args)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(parsed.Receipt)
	if err != nil {
		return fmt.Errorf("read launch receipt: %w", err)
	}
	var receipt launch.Receipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		return fmt.Errorf("decode launch receipt: %w", err)
	}
	proof, err := launchProvenanceFromReceipt(receipt)
	if err != nil {
		return err
	}
	root, _, err := gitroot.ProjectRoot(context.Background(), ".")
	if err != nil {
		root = "."
	}
	opts := reviewledger.HostIngestOpts{
		SHA: parsed.Candidate, Reviewer: parsed.Reviewer, Receipt: proof,
		ProductionBase: parsed.ProductionBase,
		CommitTime:     commitTimeOf(root, parsed.Candidate),
		Reaches:        func(branch, sha string) bool { return branchReaches(root, branch, sha) },
	}
	if parsed.Artifact != "" {
		body, err := os.ReadFile(parsed.Artifact)
		if err != nil {
			return fmt.Errorf("read artifact: %w", err)
		}
		a := reviewingest.Parse(string(body))
		if a.SHA != parsed.Candidate || a.Reviewer != parsed.Reviewer {
			return fmt.Errorf("SHA/reviewer mismatch: artifact sha=%s reviewer=%s", a.SHA, a.Reviewer)
		}
		sum := sha256.Sum256(body)
		opts.Task = reviewledger.CloseableCardRef(a.TaskRef)
		opts.Branch = a.Branch
		opts.Artifact = parsed.Artifact
		opts.ArtifactDigest = fmt.Sprintf("%x", sum)
		opts.Verdict = reviewledger.Verdict(a.Verdict)
		opts.ReviewerFamily = a.ReviewerFamily
		opts.BuilderFamily = a.BuilderFamily
		opts.VfyDigest = a.VerificationDigest()
		opts.ReadBase = a.ReadBase
		opts.ReadHead = a.ReadHead
	} else {
		opts.Task = reviewledger.CloseableCardRef(receipt.TaskRef)
		opts.Branch = receipt.Branch
	}

	ledger, err := reviewledger.NewReviewLedger(root, reviewLedgerPath())
	if err != nil {
		return err
	}
	enqueued, err := ledger.HostIngest(opts)
	if err != nil {
		return err
	}
	fmt.Printf("host-ingest: sha=%s reviewer=%s host=%s enqueued=%v\n", parsed.Candidate, parsed.Reviewer, proof.Host, enqueued)
	return nil
}
