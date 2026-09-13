package sync

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

// FAC-132: a board card reaches Done only from a durable, task-bound
// completion receipt proving that the EXACT accepted candidate was verified,
// independently reviewed, integrated into origin/main, and read back from the
// provider.
//
// WHY: the original merge-evidence oracle accepted any origin/main commit
// naming the ref, or any operator-supplied ancestor. That oracle closed
// FAC-107, FAC-108, FAC-111, FAC-114, FAC-116, FAC-128 and FAC-129 while their
// stated acceptance criteria were still unmet — an empty commit whose subject
// says "FAC-116" is indistinguishable, to a grep, from the work itself.
// Commit-message matches are now DIAGNOSTIC HINTS ONLY (see commitHint and
// AuditDone); they carry no closing authority. The sound "did this merge?"
// check is LandedProof (rebase onto origin/main, require empty diff).
//
// The content binding is the patch ID: the receipt names the patch ID of the
// accepted candidate, and Validate recomputes it from the merge commit that is
// actually on origin/main. An empty commit has no patch and cannot produce
// one, so it can never satisfy a receipt.

// CompletionReceiptVersion is the only receipt schema this build accepts.
const CompletionReceiptVersion = 1

const ProvenanceReduced = "reduced"

// CompletionReceipt is the task-bound proof of completion. It is produced by
// whatever integrated the candidate (the harvest/merge pipeline) and consumed
// exactly once by BoardDone.
//
// Provider readback is deliberately NOT a receipt field: the integrator does
// not talk to the board, so it cannot honestly attest to a readback it never
// performed. Readback is observed and recorded at consumption time, in
// DoneRecord.ProviderReadback, and a write whose readback does not say "done"
// is a hard failure (see BoardDone).
type CompletionReceipt struct {
	Version          int    `json:"version"`
	RepoID           string `json:"repo_id"`
	TaskRef          string `json:"task_ref"`
	TaskID           string `json:"task_id"`
	ProviderRevision string `json:"provider_revision"`
	LeaseGeneration  int64  `json:"lease_generation"`
	BaseSHA          string `json:"base_sha"`
	CandidateSHA     string `json:"candidate_sha"`
	MergeSHA         string `json:"merge_sha"`
	// ContentSHA is the commit whose patch PatchID identifies, when that is
	// NOT the merge commit itself.
	//
	// A pull-request landing integrates reviewed work with a merge commit
	// whose own diff is empty, so PatchID cannot be the merge's. Empty means
	// the ordinary shape where the merge carries the patch, and the pre-image
	// below omits it, so every receipt minted before this field existed keeps
	// its digest byte for byte. Set, it is SEALED by the digest like every
	// other field and is verified by Validate against the merge it claims to
	// be carried by -- it is a narrowing of the content gate, never a way
	// around it.
	ContentSHA            string `json:"content_sha,omitempty"`
	PatchID               string `json:"patch_id"`
	AcceptanceDigest      string `json:"acceptance_digest"`
	AcceptanceEvidence    string `json:"acceptance_evidence,omitempty"`
	VerificationDigest    string `json:"verification_digest"`
	RiskTier              string `json:"risk_tier"`
	AuthorFamily          string `json:"author_family"`
	ReviewerFamily        string `json:"reviewer_family"`
	Verdict               string `json:"verdict"`
	IntegrationResult     string `json:"integration_result"`
	ProvenanceMode        string `json:"provenance_mode,omitempty"`
	PullRequest           int    `json:"pull_request,omitempty"`
	ReconstructedSHA      string `json:"reconstructed_sha,omitempty"`
	ReconstructionBaseSHA string `json:"reconstruction_base_sha,omitempty"`
	ReconstructionDigest  string `json:"reconstruction_digest,omitempty"`
	Digest                string `json:"digest"`
}

// receiptForDigest is the canonical digest pre-image: every field except
// Digest itself, in declaration order.
type receiptForDigest struct {
	Version               int    `json:"version"`
	RepoID                string `json:"repo_id"`
	TaskRef               string `json:"task_ref"`
	TaskID                string `json:"task_id"`
	ProviderRevision      string `json:"provider_revision"`
	LeaseGeneration       int64  `json:"lease_generation"`
	BaseSHA               string `json:"base_sha"`
	CandidateSHA          string `json:"candidate_sha"`
	MergeSHA              string `json:"merge_sha"`
	ContentSHA            string `json:"content_sha,omitempty"`
	PatchID               string `json:"patch_id"`
	AcceptanceDigest      string `json:"acceptance_digest"`
	AcceptanceEvidence    string `json:"acceptance_evidence,omitempty"`
	VerificationDigest    string `json:"verification_digest"`
	RiskTier              string `json:"risk_tier"`
	AuthorFamily          string `json:"author_family"`
	ReviewerFamily        string `json:"reviewer_family"`
	Verdict               string `json:"verdict"`
	IntegrationResult     string `json:"integration_result"`
	ProvenanceMode        string `json:"provenance_mode,omitempty"`
	PullRequest           int    `json:"pull_request,omitempty"`
	ReconstructedSHA      string `json:"reconstructed_sha,omitempty"`
	ReconstructionBaseSHA string `json:"reconstruction_base_sha,omitempty"`
	ReconstructionDigest  string `json:"reconstruction_digest,omitempty"`
}

// ComputeDigest returns SHA-256 over the canonical JSON form of the receipt
// with Digest omitted.
func (r CompletionReceipt) ComputeDigest() string {
	b, _ := json.Marshal(receiptForDigest{
		Version: r.Version, RepoID: r.RepoID, TaskRef: r.TaskRef, TaskID: r.TaskID,
		ProviderRevision: r.ProviderRevision, LeaseGeneration: r.LeaseGeneration,
		BaseSHA: r.BaseSHA, CandidateSHA: r.CandidateSHA, MergeSHA: r.MergeSHA,
		ContentSHA: r.ContentSHA,
		PatchID:    r.PatchID, AcceptanceDigest: r.AcceptanceDigest,
		AcceptanceEvidence: r.AcceptanceEvidence,
		VerificationDigest: r.VerificationDigest, RiskTier: r.RiskTier,
		AuthorFamily: r.AuthorFamily, ReviewerFamily: r.ReviewerFamily,
		Verdict: r.Verdict, IntegrationResult: r.IntegrationResult,
		ProvenanceMode: r.ProvenanceMode, PullRequest: r.PullRequest,
		ReconstructedSHA: r.ReconstructedSHA, ReconstructionBaseSHA: r.ReconstructionBaseSHA, ReconstructionDigest: r.ReconstructionDigest,
	})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Seal stamps the schema version and the self-digest. Producers call this
// last; any later field edit invalidates the digest and Validate refuses it.
func (r *CompletionReceipt) Seal() {
	r.Version = CompletionReceiptVersion
	r.Digest = r.ComputeDigest()
}

var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// LifecycleAuthority is the durable task-state read model BoardDone consults.
// *lifecycle.EventStore satisfies it. A nil authority, a read error, or an
// absent state all refuse the transition: no state, no automatic Done.
type LifecycleAuthority interface {
	CurrentState(taskRef string) (*lifecycle.TaskState, error)
}

// Validate fails closed on the first thing it cannot prove. ref is the card
// being closed; st is the task's current durable lifecycle state.
//
// Every check answers one question: does this receipt describe THIS task, in
// THIS repository, at THIS lease generation, naming a candidate whose content
// is provably what landed on origin/main?
func (r CompletionReceipt) Validate(repoDir, ref string, st *lifecycle.TaskState) error {
	if r.Version != CompletionReceiptVersion {
		return fmt.Errorf("receipt version %d is not %d", r.Version, CompletionReceiptVersion)
	}
	if r.Digest == "" || r.Digest != r.ComputeDigest() {
		return fmt.Errorf("receipt digest does not match its contents (tampered or hand-edited)")
	}

	if r.ReconstructedSHA != "" || r.ReconstructionBaseSHA != "" || r.ReconstructionDigest != "" {
		proof, err := hex.DecodeString(r.ReconstructionDigest)
		if r.ProvenanceMode != ProvenanceReduced || !fullSHA.MatchString(r.ReconstructedSHA) || !fullSHA.MatchString(r.ReconstructionBaseSHA) || err != nil || len(proof) != 32 {
			return fmt.Errorf("receipt reconstruction binding is incomplete")
		}
	}
	ref = NormalizeRef(ref)
	if !strings.EqualFold(NormalizeRef(r.TaskRef), ref) {
		return fmt.Errorf("receipt is bound to %s, not %s", r.TaskRef, ref)
	}
	reduced := r.ProvenanceMode == ProvenanceReduced
	for _, f := range []struct{ name, val string }{
		{"task_id", r.TaskID},
		{"provider_revision", r.ProviderRevision},
		{"patch_id", r.PatchID},
		{"acceptance_digest", r.AcceptanceDigest},
		{"verification_digest", r.VerificationDigest},
		{"risk_tier", r.RiskTier},
		{"author_family", r.AuthorFamily},
		{"reviewer_family", r.ReviewerFamily},
	} {
		if reduced && (f.name == "task_id" || f.name == "provider_revision" || f.name == "acceptance_digest") {
			continue
		}
		if strings.TrimSpace(f.val) == "" {
			return fmt.Errorf("receipt is missing %s", f.name)
		}
	}
	if reduced && r.PullRequest <= 0 {
		return fmt.Errorf("reduced receipt is missing a positive pull request number")
	}
	if !reduced && r.LeaseGeneration <= 0 {
		return fmt.Errorf("receipt is missing a lease generation")
	}
	if r.Verdict != string(reviewledger.VerdictPASS) {
		return fmt.Errorf("receipt verdict is %q, not PASS", r.Verdict)
	}
	if r.IntegrationResult != IntegrationMerged {
		return fmt.Errorf("receipt integration result is %q, not %q", r.IntegrationResult, IntegrationMerged)
	}
	if !reviewledger.FamilyAllowlist[r.AuthorFamily] {
		return fmt.Errorf("author family %q is not a known builder family", r.AuthorFamily)
	}
	if !reviewledger.FamilyAllowlist[r.ReviewerFamily] {
		return fmt.Errorf("reviewer family %q is not a known builder family", r.ReviewerFamily)
	}
	if r.AuthorFamily == r.ReviewerFamily {
		return fmt.Errorf("reviewer family matches author family (self-verdict)")
	}
	for _, f := range []struct{ name, val string }{
		{"base_sha", r.BaseSHA}, {"candidate_sha", r.CandidateSHA}, {"merge_sha", r.MergeSHA},
	} {
		if !fullSHA.MatchString(f.val) {
			return fmt.Errorf("receipt %s %q is not a full 40-character commit sha", f.name, f.val)
		}
	}

	// A sealed carrier is identified as strictly as the commits beside it. An
	// empty ContentSHA is the legacy shape and stays exempt, so no receipt
	// minted before the field existed is affected.
	if r.ContentSHA != "" && !fullSHA.MatchString(r.ContentSHA) {
		return fmt.Errorf("receipt content_sha %q is not a full 40-character commit sha", r.ContentSHA)
	}

	// Repository binding: a receipt minted against another repository never
	// closes a card here, however well-formed it is.
	repoID, err := toolchild.RepositoryIdentity(repoDir)
	if err != nil {
		return fmt.Errorf("cannot resolve repository identity: %w", err)
	}
	if !strings.EqualFold(repoID, r.RepoID) {
		return fmt.Errorf("receipt is bound to repository %s, not %s", r.RepoID, repoID)
	}

	// Integration binding: the merge commit must actually be on origin/main,
	// and the base it was cut from must precede it.
	if _, err := git(repoDir, "rev-parse", "--verify", "-q", "origin/main"); err != nil {
		return fmt.Errorf("no origin/main in %s", repoDir)
	}
	if _, err := git(repoDir, "merge-base", "--is-ancestor", r.MergeSHA, "origin/main"); err != nil {
		return fmt.Errorf("merge sha %s is not an ancestor of origin/main", r.MergeSHA)
	}
	if _, err := git(repoDir, "merge-base", "--is-ancestor", r.BaseSHA, r.MergeSHA); err != nil {
		return fmt.Errorf("base sha %s is not an ancestor of merge sha %s", r.BaseSHA, r.MergeSHA)
	}

	if err := r.validateContentBinding(repoDir); err != nil {
		return err
	}
	// Reduced receipts are deliberately minted only by post-merge PR
	// reconciliation after the exact review-ledger verdict and patch-equivalent
	// landing have been proven. They omit dispatch-derived lifecycle, task-id,
	// and acceptance fields rather than fabricating them after the work landed.
	// Their positive PR, sealed digest, verification evidence, cross-family
	// PASS, repository binding, and Git/patch proof above are the completion
	// authority; an absent or expired dispatch authorization is not.
	if reduced {
		return nil
	}

	// Lifecycle binding: the task must be durably past integration, at the
	// exact lease generation and candidate the receipt was minted under.
	if st == nil {
		return fmt.Errorf("no durable lifecycle state for %s (nothing recorded this task as integrated)", ref)
	}
	if st.State != lifecycle.StateIntegrated && st.State != lifecycle.StateReconciled {
		return fmt.Errorf("lifecycle state for %s is %q, not %q or %q", ref, st.State,
			lifecycle.StateIntegrated, lifecycle.StateReconciled)
	}
	if st.LeaseGeneration != r.LeaseGeneration {
		return fmt.Errorf("receipt lease generation %d is stale: %s is at generation %d",
			r.LeaseGeneration, ref, st.LeaseGeneration)
	}
	if st.CandidateSHA != "" && st.CandidateSHA != r.CandidateSHA {
		return fmt.Errorf("receipt candidate %s is stale: %s is at candidate %s",
			r.CandidateSHA, ref, st.CandidateSHA)
	}
	return nil
}

// IntegrationMerged is the only integration result that can close a card.
const IntegrationMerged = "merged"

// PatchID returns the stable patch ID of sha's diff. A commit with no diff
// (an empty commit) is an error, not an empty patch id — that distinction is
// the whole point of the check.
func PatchID(repoDir, sha string) (string, error) {
	diff := exec.Command("git", "diff-tree", "-p", "--no-color", sha)
	diff.Dir = repoDir
	out, err := diff.Output()
	if err != nil {
		return "", fmt.Errorf("git diff-tree: %w", err)
	}
	if len(bytes.TrimSpace(out)) == 0 {
		return "", fmt.Errorf("commit carries no patch (empty commit)")
	}
	pid := exec.Command("git", "patch-id", "--stable")
	pid.Dir = repoDir
	pid.Stdin = bytes.NewReader(out)
	pout, err := pid.Output()
	if err != nil {
		return "", fmt.Errorf("git patch-id: %w", err)
	}
	fields := strings.Fields(string(pout))
	if len(fields) == 0 {
		return "", fmt.Errorf("git patch-id produced no id")
	}
	return fields[0], nil
}

// ReceiptPath is where BoardDone looks for a card's receipt by default.
func ReceiptPath(repoDir, ref string) string {
	return filepath.Join(repoDir, ".herd", "receipts", NormalizeRef(ref)+".json")
}

// MissingCompletionReceiptError names the absent artifact class and the exact
// path searched. Keep this diagnostic shared by every automatic close entry
// point: a launch receipt or task context is authorization to work, not proof
// that the work landed.
func MissingCompletionReceiptError(repoDir, ref string) error {
	ref = NormalizeRef(ref)
	return fmt.Errorf("%w for %s: no landing completion receipt at %s (completion-evidence store searched). "+
		"A commit naming the ref is a discovery hint, not proof. Supply the receipt the integration produced, "+
		"or close it manually with --override-policy/--override-actor/--override-reason/--override-evidence",
		ErrNoEvidence, ref, ReceiptPath(repoDir, ref))
}

// WriteReceipt seals and durably writes a receipt for ref. Producers use this;
// it is the seam the integration pipeline writes through.
func WriteReceipt(repoDir string, r *CompletionReceipt) error {
	return persistReceipt(repoDir, r, "", nil)
}

// LoadReceipt reads a receipt from disk. It does not validate it.
func LoadReceipt(path string) (*CompletionReceipt, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read receipt: %w", err)
	}
	var r CompletionReceipt
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("parse receipt %s: %w", path, err)
	}
	return &r, nil
}

// ---- append-only done log ----

// DoneRecord is one append-only entry in the board-done log: the durable
// statement that a card was closed, by what authority, and what the provider
// actually read back afterwards.
type DoneRecord struct {
	Timestamp        string          `json:"ts"`
	Ref              string          `json:"ref"`
	TaskID           string          `json:"task_id"`
	ReceiptDigest    string          `json:"receipt_digest,omitempty"`
	MergeSHA         string          `json:"merge_sha,omitempty"`
	ProviderReadback string          `json:"provider_readback"`
	Override         *OverrideRecord `json:"override,omitempty"`
}

// OverrideRecord is the attributable manual-override decision. Every field is
// required, and the policy must be one the build knows about.
type OverrideRecord struct {
	Actor    string `json:"actor"`
	Reason   string `json:"reason"`
	Evidence string `json:"evidence"`
	Policy   string `json:"policy"`
	Decision string `json:"decision"`

	// Route records WHICH authority closed the card: an acceptance block plus
	// literal output, or admitted cross-family review evidence for a legacy
	// card. An audit that cannot tell the two apart cannot tell prospective
	// evidence from retrospective.
	Route string `json:"route,omitempty"`

	// Legacy* are the admitted review facts when Route is the legacy route.
	// They are copied from the review ledger, never supplied by the closer.
	LegacyCandidateSHA   string `json:"legacy_candidate_sha,omitempty"`
	LegacyArtifact       string `json:"legacy_artifact,omitempty"`
	LegacyReviewer       string `json:"legacy_reviewer,omitempty"`
	LegacyReviewerFamily string `json:"legacy_reviewer_family,omitempty"`
	LegacyBuilderFamily  string `json:"legacy_builder_family,omitempty"`
	LegacyMergeSHA       string `json:"legacy_merge_sha,omitempty"`
}

// OverrideRequest is what an operator supplies to close a card without a
// receipt.
type OverrideRequest struct {
	Actor    string
	Reason   string
	Evidence string
	Policy   string
}

// OverridePolicies is the closed set of standing exceptions under which a card
// may be closed without a receipt. Anything not named here is refused: an
// override is an exercise of a written policy, not a free-form escape hatch.
var OverridePolicies = map[string]string{
	"operator-external-merge": "work provably landed on origin/main outside the fleet integration path",
	"duplicate-card":          "card duplicates another card already closed by a receipt",
	"abandoned-scope":         "scope was withdrawn; the card is closed as not-to-be-built",
}

// authorizeOverride turns an operator request into a recordable decision, or
// refuses it. Missing attribution and unknown policies both refuse.
func authorizeOverride(req OverrideRequest) (*OverrideRecord, error) {
	for _, f := range []struct{ name, val string }{
		{"actor", req.Actor}, {"reason", req.Reason}, {"evidence", req.Evidence}, {"policy", req.Policy},
	} {
		if strings.TrimSpace(f.val) == "" {
			return nil, fmt.Errorf("manual override requires %s", f.name)
		}
	}
	decision, ok := OverridePolicies[req.Policy]
	if !ok {
		return nil, fmt.Errorf("manual override policy %q is not one of the permitted policies: %s",
			req.Policy, strings.Join(sortedPolicies(), ", "))
	}
	return &OverrideRecord{
		Actor: req.Actor, Reason: req.Reason, Evidence: req.Evidence,
		Policy: req.Policy, Decision: decision,
	}, nil
}

// SortedOverridePolicies lists the permitted policy names deterministically.
func SortedOverridePolicies() []string { return sortedPolicies() }

func sortedPolicies() []string {
	out := make([]string, 0, len(OverridePolicies))
	for k := range OverridePolicies {
		out = append(out, k)
	}
	// Small fixed set; a plain insertion sort keeps the message deterministic.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// DoneLogPath is the append-only board-done log.
func DoneLogPath(repoDir string) string {
	return filepath.Join(repoDir, ".herd", "board-done.jsonl")
}

// appendDoneRecord appends one line. The file is only ever opened O_APPEND —
// nothing in this package truncates, rewrites, or deletes it.
func appendDoneRecord(repoDir string, rec DoneRecord) error {
	path := DoneLogPath(repoDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("done log dir: %w", err)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("encode done record: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open done log: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("append done log: %w", err)
	}
	return f.Sync()
}

// ReadDoneLog returns every recorded closure, oldest first. A missing log is
// not an error (nothing has been closed under this authority yet); an
// unparseable line IS an error, so a corrupt log can never read as "no record"
// and silently license a re-close.
func ReadDoneLog(repoDir string) ([]DoneRecord, error) {
	b, err := os.ReadFile(DoneLogPath(repoDir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read done log: %w", err)
	}
	var out []DoneRecord
	for i, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec DoneRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			return nil, fmt.Errorf("done log line %d is not valid JSON: %w", i+1, err)
		}
		out = append(out, rec)
	}
	return out, nil
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

// The content-binding proof answers one question -- is the merged tree the
// result of the REVIEWED DELTA -- and it has to answer inside a FIXED PHYSICAL
// BUDGET. It runs in `herd approve` against a repository whose history an author
// influences, so an unbounded walk is a denial of service with a receipt
// attached. Per-command timeouts and argv batching bound neither the number of
// commands nor the total work, so the budget below is shared by the whole proof.
//
// Every limit fails CLOSED through ErrContentProofBudget. A proof that was
// STOPPED is never reported as a landing that was PRESERVED: the two are
// different answers and the caller must be able to tell them apart.
const (
	contentProofTimeout        = 30 * time.Second
	contentProofKillDelay      = 5 * time.Second
	contentProofMaxCommands    = 16
	contentProofMaxOutputBytes = 1 << 20
	contentProofMaxStderrBytes = 8 << 10
)

// ErrContentProofBudget is the one sentinel for every limit above.
var ErrContentProofBudget = errors.New("content proof stopped by its budget")

var errContentProofOutput = errors.New("git produced more output than the content proof allows")

// boundedOutput refuses to grow past max and reports that as a write error,
// which exec surfaces from Wait. Capping AT THE PIPE is the point: measuring
// output after reading all of it has already spent the memory.
type boundedOutput struct {
	max        int
	text       strings.Builder
	overflowed bool
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	room := w.max - w.text.Len()
	if room < 0 {
		room = 0
	}
	if room < len(p) {
		if room > 0 {
			w.text.Write(p[:room])
		}
		w.overflowed = true
		return room, errContentProofOutput
	}
	w.text.Write(p)
	return len(p), nil
}

// contentProofCommand is the subprocess seam. It exists so the budgets can be
// proven by a deterministic test -- an injected command that returns more bytes
// than the cap, or that is asked for once too often -- rather than by hoping the
// machine running CI is slow enough or the repository large enough.
var contentProofCommand = runBoundedGit

// runBoundedGit is the physical half of the same budget. The shared deadline
// kills the process, WaitDelay stops a child that ignores that from holding the
// call open, and stdout and stderr have separate capped writers so neither can
// grow without limit and the two goroutines exec uses never share one buffer.
func runBoundedGit(ctx context.Context, repoDir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repoDir
	cmd.WaitDelay = contentProofKillDelay
	stdout := &boundedOutput{max: contentProofMaxOutputBytes}
	stderr := &boundedOutput{max: contentProofMaxStderrBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	if stdout.overflowed || stderr.overflowed {
		return "", fmt.Errorf("%w: git %s produced more than the %d byte cap",
			ErrContentProofBudget, strings.Join(args, " "), contentProofMaxOutputBytes)
	}
	return strings.TrimSpace(stdout.text.String()), err
}

// contentProof holds the budget for ONE validation. Every git call the proof
// makes goes through run -- including the ones the shared replay primitive
// makes, because the primitive takes this method as its runner and starts no
// process of its own -- so the counters cannot be bypassed by adding a code path.
type contentProof struct {
	repoDir  string
	ctx      context.Context
	commands int
}

func (p *contentProof) run(args ...string) (string, error) {
	if p.commands >= contentProofMaxCommands {
		return "", fmt.Errorf("%w: the proof asked for more than %d git commands",
			ErrContentProofBudget, contentProofMaxCommands)
	}
	if err := p.ctx.Err(); err != nil {
		return "", fmt.Errorf("%w: %w", ErrContentProofBudget, err)
	}
	p.commands++
	out, err := contentProofCommand(p.ctx, p.repoDir, args...)
	if len(out) > contentProofMaxOutputBytes {
		return "", fmt.Errorf("%w: git %s returned %d bytes, more than the %d byte cap",
			ErrContentProofBudget, strings.Join(args, " "), len(out), contentProofMaxOutputBytes)
	}
	if err != nil {
		// A failure while the shared deadline is gone is the budget speaking,
		// not evidence about content.
		if ctxErr := p.ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("%w: %w", ErrContentProofBudget, ctxErr)
		}
	}
	return out, err
}

// reviewedDelta names the two commits whose difference IS the reviewed content.
//
// Ordinarily that is the sealed base and candidate. A RECONSTRUCTED receipt
// seals the original reviewed identities, which may no longer exist as objects,
// alongside the reconstruction that does -- and the producer proved its landing
// against the reconstruction, so the consumer must replay the same pair or it is
// checking a different claim. Both come from the receipt and both are covered by
// its digest: nothing here is chosen by the caller, and no descendant, branch
// head or working tree is ever substituted for the sealed candidate.
func (r CompletionReceipt) reviewedDelta() (base, candidate string) {
	if r.ReconstructedSHA != "" && r.ReconstructionBaseSHA != "" {
		return r.ReconstructionBaseSHA, r.ReconstructedSHA
	}
	return r.BaseSHA, r.CandidateSHA
}

// mergedTreeIsTheReviewedResult is the content gate for a sealed carrier.
//
// It asks the producer's own question, with the producer's own command: replay
// the reviewed delta onto the integration commit's FIRST PARENT and require the
// result to equal the integration commit's tree. That is a WHOLE-RESULT claim,
// which is why it answers the shapes a path-by-path comparison could not:
//
//   - a reviewed commit that renames a file AND edits it in the same commit: the
//     replay reproduces both, where following the old path by name or by blob
//     could not establish the destination at all;
//   - a file where main and the reviewed line changed DIFFERENT hunks: the
//     replay lands on main's tip, so the merged result is what it produces --
//     no longer a false refusal;
//   - later revisions inside the pull request: they are part of the delta.
//
// And it refuses the shapes that matter: an ours merge discards the reviewed
// hunks so its tree is its first parent's, not the replay's; a merge amended to
// substitute bytes holds neither; and a receipt whose sealed candidate was
// swapped for another commit replays a different delta and lands a different
// tree. A replay that cannot be computed at all -- a conflict -- is a refusal,
// never an assumption.
func (p *contentProof) mergedTreeIsTheReviewedResult(r CompletionReceipt) error {
	base, candidate := r.reviewedDelta()
	// An integration commit that IS the reviewed candidate has no independent
	// reviewed delta to replay: the claim would prove itself. The producer never
	// seals a carrier in that shape, and refusing it here is what stops a
	// receipt from naming the merge as its own candidate.
	if strings.EqualFold(candidate, r.MergeSHA) {
		return fmt.Errorf(
			"merge sha %s does not preserve the content of %s: the reviewed candidate is the integration commit itself, so there is no reviewed delta to replay",
			r.MergeSHA, r.ContentSHA)
	}
	parent, err := p.run("rev-parse", "--verify", "-q", r.MergeSHA+"^1")
	if err != nil {
		if errors.Is(err, ErrContentProofBudget) {
			return err
		}
		return fmt.Errorf(
			"merge sha %s does not preserve the content of %s: it has no first parent to replay onto: %w",
			r.MergeSHA, r.ContentSHA, err)
	}
	replayed, err := gitroot.ReplayReviewedTree(base, parent, candidate, p.run)
	if err != nil {
		if errors.Is(err, ErrContentProofBudget) {
			return err
		}
		return fmt.Errorf(
			"merge sha %s does not preserve the content of %s: replaying the reviewed delta %s..%s onto %s did not produce a tree: %w",
			r.MergeSHA, r.ContentSHA, base, candidate, parent, err)
	}
	landedTree, err := p.run("rev-parse", "--verify", "-q", r.MergeSHA+"^{tree}")
	if err != nil {
		if errors.Is(err, ErrContentProofBudget) {
			return err
		}
		return fmt.Errorf("merge sha %s: cannot read its own tree: %w", r.MergeSHA, err)
	}
	if !strings.EqualFold(replayed, landedTree) {
		return fmt.Errorf(
			"merge sha %s does not preserve the content of %s: replaying the reviewed delta %s..%s onto %s produces tree %s, and the merged tree is %s",
			r.MergeSHA, r.ContentSHA, base, candidate, parent, replayed, landedTree)
	}
	return nil
}

// replayProvesTheMergedTree is the consumer entry point: one shared deadline for
// the whole proof, cancelled on every return.
func (r CompletionReceipt) replayProvesTheMergedTree(repoDir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), contentProofTimeout)
	defer cancel()
	proof := &contentProof{repoDir: repoDir, ctx: ctx}
	return proof.mergedTreeIsTheReviewedResult(r)
}

// validateContentBinding is the content half of Validate, extracted so the
// contract can be driven directly by a test without fabricating every unrelated
// field a full receipt carries. Validate calls it and nothing else does: this
// is the shipped consumer path, not a copy of it.
func (r CompletionReceipt) validateContentBinding(repoDir string) error {
	// Content binding: the accepted candidate's patch must actually be in what
	// landed. This is what makes "an empty commit naming the ticket" useless —
	// an empty commit has no patch id at all.
	//
	// Ordinary landing: the merge commit carries the patch, and this is exactly
	// the check it always was. Pull-request landing: the merge integrates the
	// work and its OWN diff is empty, so the patch belongs to the carrier the
	// receipt seals in ContentSHA. That does not relax the gate, it moves it
	// onto the object that actually holds the content and adds two more
	// requirements the ordinary shape never needed.
	contentSHA := r.MergeSHA
	if r.ContentSHA != "" {
		contentSHA = r.ContentSHA
		// Ancestry: the carrier must be IN the merge it claims to be carried
		// by. Without this a receipt could name any commit in the repository
		// whose patch happens to match and pass.
		if _, err := git(repoDir, "merge-base", "--is-ancestor", r.ContentSHA, r.MergeSHA); err != nil {
			return fmt.Errorf("content sha %s is not an ancestor of merge sha %s", r.ContentSHA, r.MergeSHA)
		}
		// Content: the merged tree must BE the reviewed result -- the sealed
		// reviewed delta replayed onto the integration commit's first parent.
		// Ancestry alone is satisfied by an ours merge that discarded every
		// reviewed hunk, and by a merge amended to substitute different bytes:
		// both keep the carrier an ancestor while landing something else. The
		// carrier keeps its own independent bindings -- that ancestry, and the
		// patch id below -- and the replay runs inside one shared, finite budget.
		if err := r.replayProvesTheMergedTree(repoDir); err != nil {
			return err
		}
	}
	landed, err := PatchID(repoDir, contentSHA)
	if err != nil {
		return fmt.Errorf("content sha %s: %w", contentSHA, err)
	}
	if landed != r.PatchID {
		return fmt.Errorf("content sha %s carries patch %s, not the accepted candidate's patch %s",
			contentSHA, landed, r.PatchID)
	}
	return nil
}
