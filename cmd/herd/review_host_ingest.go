package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/committime"
	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/launch"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/toolchild"
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
		if a == "--host" || strings.HasPrefix(a, "--host=") || a == "--family" || strings.HasPrefix(a, "--family=") {
			return out, fmt.Errorf("%s is not authentication; host and family must come from the canonical accepted launch log", strings.SplitN(a, "=", 2)[0])
		}
		if v, next, ok, err := takeCLIFlag(args, i, "--candidate"); ok {
			if err != nil {
				return out, usage
			}
			out.Candidate = v
			i = next
			continue
		}
		if v, next, ok, err := takeCLIFlag(args, i, "--reviewer"); ok {
			if err != nil {
				return out, usage
			}
			out.Reviewer = v
			i = next
			continue
		}
		if v, next, ok, err := takeCLIFlag(args, i, "--receipt"); ok {
			if err != nil {
				return out, usage
			}
			out.Receipt = v
			i = next
			continue
		}
		if v, next, ok, err := takeCLIFlag(args, i, "--artifact"); ok {
			if err != nil {
				return out, usage
			}
			out.Artifact = v
			i = next
			continue
		}
		if v, next, ok, err := takeCLIFlag(args, i, "--base"); ok {
			if err != nil {
				return out, usage
			}
			out.ProductionBase = v
			i = next
			continue
		}
		switch {
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

func resolveCanonicalLaunchProvenance(root string, locator launch.Receipt, reviewer, sha, artifactTask string, commitTime time.Time, reaches func(branch, sha string) bool) (reviewledger.LaunchProvenance, error) {
	path := launch.ReceiptPathFor(root)
	members, err := launch.ReadReceipts(path)
	if err != nil {
		return reviewledger.LaunchProvenance{}, fmt.Errorf("read canonical launch log: %w", err)
	}
	if len(members) == 0 {
		return reviewledger.LaunchProvenance{}, fmt.Errorf("canonical launch log is missing")
	}
	member, err := launch.AcceptedCanonicalMember(members, locator)
	if err != nil {
		return reviewledger.LaunchProvenance{}, err
	}
	task, repo, lane, err := expectedReviewLaunchBinding(root, reviewer, artifactTask, member)
	if err != nil {
		return reviewledger.LaunchProvenance{}, err
	}
	reviewLaunch, err := launch.AcceptedReviewLaunchForCandidate(members, reviewer, sha, task, repo, lane)
	if err != nil {
		return reviewledger.LaunchProvenance{}, err
	}
	host := reviewledger.HostFromLaunchProof(reviewLaunch.ProcessIdentity, reviewLaunch.HerdrSession, reviewLaunch.PaneID, reviewLaunch.CWD, reviewLaunch.Worktree)
	if host == "" {
		return reviewledger.LaunchProvenance{}, fmt.Errorf("canonical review launch does not authenticate a host or session")
	}
	builder, ok := launch.ReachingBuilderReceipt(path, sha, commitTime, func(branch string) bool {
		return reaches != nil && reaches(branch, sha)
	})
	if !ok {
		return reviewledger.LaunchProvenance{}, fmt.Errorf("canonical launch log does not authenticate a reaching builder family")
	}
	family := strings.TrimSpace(builder.BuilderFamily)
	if family == "" {
		return reviewledger.LaunchProvenance{}, fmt.Errorf("canonical launch log does not authenticate a reaching builder family")
	}
	return reviewledger.LaunchProvenance{
		CandidateSHA:  strings.TrimSpace(sha),
		Host:          host,
		Session:       firstNonEmptyCLI(reviewLaunch.ProcessIdentity, reviewLaunch.HerdrSession, reviewLaunch.PaneID),
		BuilderFamily: family,
		Branch:        strings.TrimSpace(builder.Branch),
		CreatedAt:     builder.CreatedAt,
		Accepted:      true,
		Member:        true,
	}, nil
}

func expectedReviewLaunchBinding(root, reviewer, artifactTask string, member launch.Receipt) (task, repo, lane string, err error) {
	repo, err = toolchild.RepositoryIdentity(root)
	if err != nil || strings.TrimSpace(repo) == "" {
		return "", "", "", fmt.Errorf("canonical review launch requires repository identity")
	}
	task = strings.TrimSpace(artifactTask)
	if task == "" {
		task = strings.TrimSpace(member.TaskRef)
	}
	if task == "" {
		return "", "", "", fmt.Errorf("canonical review launch requires task binding")
	}
	lane = strings.TrimSpace(reviewer)
	if lane == "" {
		return "", "", "", fmt.Errorf("canonical review launch requires lane binding")
	}
	return task, repo, lane, nil
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
	return committime.Of(root, sha)
}

func branchReaches(root, branch, sha string) bool {
	return gitroot.RequireAncestor(root, sha, branch) == nil
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
	var locator launch.Receipt
	if err := json.Unmarshal(raw, &locator); err != nil {
		return fmt.Errorf("decode launch receipt: %w", err)
	}
	root, _, err := gitroot.ProjectRoot(context.Background(), ".")
	if err != nil {
		root = "."
	}
	var artifact reviewingest.Artifact
	var artifactBody []byte
	task := ""
	if parsed.Artifact != "" {
		body, err := os.ReadFile(parsed.Artifact)
		if err != nil {
			return fmt.Errorf("read artifact: %w", err)
		}
		artifact = reviewingest.Parse(string(body))
		if artifact.SHA != parsed.Candidate || artifact.Reviewer != parsed.Reviewer {
			return fmt.Errorf("SHA/reviewer mismatch: artifact sha=%s reviewer=%s", artifact.SHA, artifact.Reviewer)
		}
		artifactBody = body
		task = reviewledger.CloseableCardRef(artifact.TaskRef)
	}
	commitTime := commitTimeOf(root, parsed.Candidate)
	reaches := func(branch, sha string) bool { return branchReaches(root, branch, sha) }
	proof, err := resolveCanonicalLaunchProvenance(root, locator, parsed.Reviewer, parsed.Candidate, task, commitTime, reaches)
	if err != nil {
		return err
	}
	opts := reviewledger.HostIngestOpts{
		SHA: parsed.Candidate, Reviewer: parsed.Reviewer, Receipt: proof,
		ProductionBase: parsed.ProductionBase,
		CommitTime:     commitTime,
		Reaches:        reaches,
	}
	if parsed.Artifact != "" {
		sum := sha256.Sum256(artifactBody)
		opts.Task = task
		opts.Branch = artifact.Branch
		opts.Artifact = parsed.Artifact
		opts.ArtifactDigest = fmt.Sprintf("%x", sum)
		opts.Verdict = reviewledger.Verdict(artifact.Verdict)
		opts.ReviewerFamily = artifact.ReviewerFamily
		opts.BuilderFamily = artifact.BuilderFamily
		opts.VfyDigest = artifact.VerificationDigest()
		opts.ReadBase = artifact.ReadBase
		opts.ReadHead = artifact.ReadHead
	} else {
		opts.Branch = proof.Branch
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
