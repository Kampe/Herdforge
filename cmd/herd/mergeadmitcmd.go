package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/provider"
	"github.com/Kampe/Herdforge/pkg/remoteci"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// FAC-156: the coordinator's merge step is compiled code, reachable from a
// shell, instead of a hand-run `gh pr merge` guarded by a grep.
//
// Two phases, with a DURABLE artefact between them:
//
//	herd merge-admit    --ref FAC-x ... --pr 151   → decides, persists the decision
//	<the merge happens>
//	herd merge-complete --ref FAC-x                → proves it landed, mints the receipt
//
// The decision is written to disk because "Done must consume the durable
// compiled result, never an unconditional success string". merge-complete
// reads that file; it cannot be talked into a receipt by an argument.

// admissionRecord is the durable handoff between the two phases.
type admissionRecord struct {
	Request  mergeadmit.Request  `json:"request"`
	Decision mergeadmit.Decision `json:"decision"`
}

func admissionRecordPath(repoDir, ref string) string {
	return filepath.Join(repoDir, ".herd", "merge-admissions", hsync.NormalizeRef(ref)+".json")
}

// mergeAdmitFlags registers the caller-asserted merge context. Flags are
// looked up BY NAME, never by argv position — FAC-138 shipped a bug where a
// flag after a positional silently parsed as its zero value.
type mergeAdmitFlags struct {
	ref, taskID, candidate, base     *string
	lease, patchID, acceptance, mode *string
	authorFamily, authorIdentity     *string
	leaseGeneration                  *int64
	pr                               *int
	remoteCIAttempt                  *int64
	remoteCIFile                     *string
	asJSON                           *bool
}

func registerMergeAdmitFlags(fs *flag.FlagSet) *mergeAdmitFlags {
	return &mergeAdmitFlags{
		ref:             fs.String("ref", "", "Board ticket ref (required)"),
		taskID:          fs.String("task-id", "", "Provider task id the work is bound to (required)"),
		candidate:       fs.String("candidate", "", "Exact reviewed candidate sha (required)"),
		base:            fs.String("base", "", "Base sha the candidate was reviewed against (required)"),
		lease:           fs.String("lease", "", "Claim lease token (required)"),
		leaseGeneration: fs.Int64("lease-generation", 0, "Claim lease generation (required, positive)"),
		patchID:         fs.String("patch-id", "", "Patch identity bound into the reviewer's verdict (required)"),
		acceptance: fs.String("acceptance-digest", "",
			"Acceptance digest carried from review time (required). Compute with `herd merge-admit --show-acceptance --ref X`."),
		authorFamily:    fs.String("author-family", "", "Builder model family (required)"),
		authorIdentity:  fs.String("author-identity", "", "Builder session identity (required)"),
		mode:            fs.String("mode", "", "How the merge will be published: merge, rebase, or squash (required)"),
		pr:              fs.Int("pr", 0, "Pull request number to probe for head/mergeability/CI (required)"),
		remoteCIAttempt: fs.Int64("remote-ci-attempt", 0, "Remote CI attempt bound to this candidate (required)"),
		remoteCIFile:    fs.String("remote-ci-file", ".herd/remote-ci.jsonl", "Durable remote CI settlement ledger"),
		asJSON:          fs.Bool("json", false, "Emit the decision as JSON"),
	}
}

func (f *mergeAdmitFlags) request() mergeadmit.Request {
	return mergeadmit.Request{
		Ref: *f.ref, TaskID: *f.taskID, CandidateSHA: *f.candidate, BaseSHA: *f.base,
		Lease: *f.lease, LeaseGeneration: *f.leaseGeneration, PatchURL: *f.patchID,
		AcceptanceDigest: *f.acceptance, AuthorFamily: *f.authorFamily,
		AuthorIdentity: *f.authorIdentity, Mode: mergeadmit.Mode(*f.mode),
	}
}

// runMergeAdmit is phase one: decide, and persist the decision.
func runMergeAdmit() {
	fs := flag.NewFlagSet("merge-admit", flag.ExitOnError)
	mf := registerMergeAdmitFlags(fs)
	showAcceptance := fs.Bool("show-acceptance", false,
		"Print the acceptance digest for the card's CURRENT revision and exit")
	fs.Parse(os.Args[2:])

	if *showAcceptance {
		digest, revision, err := liveAcceptanceDigest(*mf.ref, *mf.taskID)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd merge-admit: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s\n", digest)
		fmt.Fprintf(os.Stderr, "herd merge-admit: digest binds %s at card revision %s\n", *mf.ref, revision[:12])
		return
	}

	gate, err := buildMergeGate(*mf.ref, *mf.taskID, *mf.pr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REFUSED  [%s] %s: %v\n", hsync.NormalizeRef(*mf.ref), mergeadmit.CodeRemoteCI, err)
		os.Exit(1)
	}

	req := mf.request()
	if err := bindRemoteCIAdmission(gate, &req, *mf.remoteCIFile, *mf.remoteCIAttempt); err != nil {
		fmt.Fprintf(os.Stderr, "herd merge-admit: %v\n", err)
		os.Exit(1)
	}
	decision, admitErr := gate.Admit(req)

	if *mf.asJSON {
		out, _ := json.MarshalIndent(decision, "", "  ")
		fmt.Println(string(out))
	}

	// Gate on the DECISION, not on the error, and fail closed when there is
	// somehow neither.
	if decision == nil || !decision.Admitted {
		reason := "admission refused"
		code := mergeadmit.CodeProbeFailed
		if decision != nil {
			reason, code = decision.Reason, decision.Code
		} else if admitErr != nil {
			reason = admitErr.Error()
		}
		fmt.Fprintf(os.Stderr, "REFUSED  [%s] %s: %s\n", hsync.NormalizeRef(*mf.ref), code, reason)
		os.Exit(1)
	}

	if err := writeAdmissionRecord(".", req, *decision); err != nil {
		// An admission that cannot be recorded has not happened: merge-complete
		// would have nothing durable to consume.
		fmt.Fprintf(os.Stderr, "herd merge-admit: decision could not be recorded, refusing to report success: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("ADMITTED [%s] candidate %s on base %s (mode %s)\n  reviewer %s (%s), tier %s\n  %s\n",
		decision.Ref, shortSHA12(decision.CandidateSHA), shortSHA12(decision.BaseSHA), decision.Mode,
		decision.Reviewer, decision.ReviewerFam, decision.Tier, decision.Reason)
	fmt.Printf("  decision recorded at %s\n", admissionRecordPath(".", decision.Ref))
	fmt.Println("  merge now, then run: herd merge-complete --ref " + decision.Ref)
}

// bindRemoteCIAdmission constructs the exact policy/repository/candidate
// binding from the declared merge policy and loads its durable settlement.
func bindRemoteCIAdmission(gate *mergeadmit.Gate, req *mergeadmit.Request, ledgerPath string, attempt int64) error {
	if gate == nil || req == nil {
		return fmt.Errorf("remote CI admission requires gate and request")
	}
	if !gate.Policy.RemoteCI.Required {
		return nil
	}
	if attempt < 1 {
		return fmt.Errorf("remote CI attempt is required and must be positive")
	}
	repo, err := toolchild.RepositoryIdentity(".")
	if err != nil {
		return fmt.Errorf("remote CI repository identity: %w", err)
	}
	checks := append([]string(nil), gate.Policy.RemoteCI.RequiredChecks...)
	sort.Strings(checks)
	policyRevision := remoteci.Revision(preflight.PolicyRevision(gate.Policy), strings.Join(checks, "\x00"))
	binding := remoteci.Binding{Repository: repo, CandidateSHA: req.CandidateSHA, PolicyRevision: policyRevision, Attempt: attempt, RequiredChecks: checks}
	gate.RemoteCIRepository, gate.RemoteCIPolicyRevision = repo, policyRevision
	store, err := remoteci.Open(ledgerPath)
	if err != nil {
		return err
	}
	settlement, err := store.Load(binding)
	if err != nil {
		return fmt.Errorf("remote CI settlement: %w", err)
	}
	req.RemoteCI = &settlement
	return nil
}

// runMergeComplete is phase two: prove the merge landed the reviewed content
// and mint the completion receipt `herd approve` consumes.
func runMergeComplete() {
	fs := flag.NewFlagSet("merge-complete", flag.ExitOnError)
	ref := fs.String("ref", "", "Board ticket ref (required)")
	asJSON := fs.Bool("json", false, "Emit the receipt as JSON")
	fs.Parse(os.Args[2:])

	if strings.TrimSpace(*ref) == "" {
		fmt.Fprintln(os.Stderr, "usage: herd merge-complete --ref <FAC-123>")
		os.Exit(2)
	}

	rec, err := readAdmissionRecord(".", *ref)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd merge-complete: %v\n", err)
		os.Exit(1)
	}

	// The gate is rebuilt from live state; nothing is trusted from the record
	// except the decision and the request it was made against.
	gate, err := buildMergeGate(rec.Request.Ref, rec.Request.TaskID, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd merge-complete: %v\n", err)
		os.Exit(1)
	}

	receipt, err := gate.Complete(&rec.Decision, rec.Request)
	if err != nil {
		fmt.Fprintf(os.Stderr, "REFUSED  [%s]: %v\n", hsync.NormalizeRef(*ref), err)
		os.Exit(1)
	}

	if *asJSON {
		out, _ := json.MarshalIndent(receipt, "", "  ")
		fmt.Println(string(out))
	}
	fmt.Printf("COMPLETED [%s] candidate %s landed as %s\n  receipt %s at %s\n",
		receipt.TaskRef, shortSHA12(receipt.CandidateSHA), shortSHA12(receipt.MergeSHA),
		shortSHA12(receipt.Digest), hsync.ReceiptPath(".", receipt.TaskRef))
	fmt.Println("  close the card with: herd approve " + receipt.TaskRef)
}

// buildMergeGate wires the compiled gate to live authorities. prNumber may be
// 0 for the completion phase, which only needs the git-side probe.
func buildMergeGate(ref, taskID string, prNumber int) (*mergeadmit.Gate, error) {
	policy, err := preflight.LoadMergePolicy(".")
	if err != nil {
		return nil, fmt.Errorf("merge policy: %w", err)
	}
	ledger, err := reviewledger.NewReviewLedger(".", filepath.Join(".herd", "review-ledger.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("open review ledger: %w", err)
	}

	live := mergeadmit.LiveState{OriginMain: originMainProbe(".")}
	if prNumber > 0 {
		pr := newPRProbes(prNumber)
		live.CandidateHead = pr.head
		live.Mergeable = pr.mergeable
		live.Checks = pr.checks
	}
	if ref != "" && taskID != "" {
		live.TaskRevision = taskRevisionProbe(ref, taskID)
	}

	return &mergeadmit.Gate{RepoDir: ".", Ledger: ledger, Policy: policy, Live: live}, nil
}

// originMainProbe reports the exact integration tip. It fetches first so the
// read is CURRENT — a stale remote-tracking ref would let a merge proceed
// against a base that moved (FAC-94's serial-train invalidation).
func originMainProbe(dir string) mergeadmit.Probe {
	return func() (string, error) {
		if out, err := exec.Command("git", "-C", dir, "fetch", "-q", "origin", "main").CombinedOutput(); err != nil {
			return "", fmt.Errorf("fetch origin/main: %v: %s", err, out)
		}
		out, err := exec.Command("git", "-C", dir, "rev-parse", "--verify", "-q", "origin/main^{commit}").Output()
		if err != nil {
			return "", fmt.Errorf("rev-parse origin/main: %w", err)
		}
		return strings.TrimSpace(string(out)), nil
	}
}

// prView is the ONE gh call the PR probes make. Everything is read from a
// single command with a single exit status and parsed in Go.
//
// This shape is the FAC-162 fix. The incident command was
//
//	gh pr view … | jq … | head -1
//
// where jq and head exit 0 over an empty stream, so a failed gh read as a
// clean gate. There is no pipeline here for a tail to overwrite the status of.
type prView struct {
	HeadRefOid        string `json:"headRefOid"`
	Mergeable         string `json:"mergeable"`
	StatusCheckRollup []struct {
		Name       string `json:"name"`
		Context    string `json:"context"`
		Conclusion string `json:"conclusion"`
		State      string `json:"state"`
	} `json:"statusCheckRollup"`
}

type prProbes struct {
	number int
	cached *prView
}

func newPRProbes(number int) *prProbes { return &prProbes{number: number} }

func (p *prProbes) view() (*prView, error) {
	if p.cached != nil {
		return p.cached, nil
	}
	cmd := exec.Command("gh", "pr", "view", fmt.Sprint(p.number),
		"--json", "headRefOid,mergeable,statusCheckRollup")
	out, err := cmd.Output()
	// The producer's exit status is checked BEFORE the output is looked at.
	if err != nil {
		return nil, fmt.Errorf("gh pr view %d: %w", p.number, err)
	}
	var v prView
	if jsonErr := json.Unmarshal(out, &v); jsonErr != nil {
		return nil, fmt.Errorf("gh pr view %d returned unparseable json: %w", p.number, jsonErr)
	}
	p.cached = &v
	return p.cached, nil
}

func (p *prProbes) head() (string, error) {
	v, err := p.view()
	if err != nil {
		return "", err
	}
	return v.HeadRefOid, nil
}

func (p *prProbes) mergeable() (string, error) {
	v, err := p.view()
	if err != nil {
		return "", err
	}
	return v.Mergeable, nil
}

func (p *prProbes) checks() (map[string]string, error) {
	v, err := p.view()
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range v.StatusCheckRollup {
		name := c.Name
		if name == "" {
			name = c.Context
		}
		if name == "" {
			continue
		}
		// Checks report `conclusion`, statuses report `state`. Take whichever
		// the provider populated; an entry with neither stays empty and is
		// refused downstream rather than skipped.
		conclusion := c.Conclusion
		if conclusion == "" {
			conclusion = c.State
		}
		out[name] = conclusion
	}
	return out, nil
}

// taskRevisionProbe reads the LIVE board card and derives its content
// revision. A card whose acceptance criteria were edited after review yields a
// different revision, which invalidates the acceptance digest.
func taskRevisionProbe(ref, taskID string) mergeadmit.Probe {
	return func() (string, error) {
		task, err := liveTask(taskID)
		if err != nil {
			return "", err
		}
		return mergeadmit.TaskContentRevision(ref, task.Title, task.Description), nil
	}
}

func liveTask(taskID string) (*provider.Task, error) {
	cfg, err := config.LoadConfig(".herd/herd.yaml")
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}
	tp, err := loadTaskProvider(cfg)
	if err != nil {
		return nil, fmt.Errorf("task provider: %w", err)
	}
	task, err := tp.GetTask(context.Background(), taskID)
	if err != nil {
		return nil, fmt.Errorf("read board card %s: %w", taskID, err)
	}
	if task == nil {
		return nil, fmt.Errorf("board card %s does not exist", taskID)
	}
	return task, nil
}

// liveAcceptanceDigest computes the digest binding a merge to the card's
// current revision, so a reviewer can record it with their verdict.
func liveAcceptanceDigest(ref, taskID string) (digest, revision string, err error) {
	if strings.TrimSpace(ref) == "" || strings.TrimSpace(taskID) == "" {
		return "", "", fmt.Errorf("--ref and --task-id are required to compute an acceptance digest")
	}
	task, err := liveTask(taskID)
	if err != nil {
		return "", "", err
	}
	revision = mergeadmit.TaskContentRevision(ref, task.Title, task.Description)
	return mergeadmit.ComputeAcceptanceDigest(ref, taskID, revision), revision, nil
}

func writeAdmissionRecord(repoDir string, req mergeadmit.Request, d mergeadmit.Decision) error {
	path := admissionRecordPath(repoDir, req.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("admission dir: %w", err)
	}
	b, err := json.MarshalIndent(admissionRecord{Request: req, Decision: d}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode admission record: %w", err)
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func readAdmissionRecord(repoDir, ref string) (*admissionRecord, error) {
	path := admissionRecordPath(repoDir, ref)
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("no recorded admission at %s: run `herd merge-admit` before merging: %w", path, err)
	}
	var rec admissionRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("admission record %s is not valid JSON: %w", path, err)
	}
	// A record that does not say "admitted" is not a licence, however it got
	// onto disk.
	if !rec.Decision.Admitted {
		return nil, fmt.Errorf("admission record %s records a REFUSAL (%s): %s",
			path, rec.Decision.Code, rec.Decision.Reason)
	}
	return &rec, nil
}

func shortSHA12(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}
