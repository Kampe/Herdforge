package sync

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

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
	Version            int    `json:"version"`
	RepoID             string `json:"repo_id"`
	TaskRef            string `json:"task_ref"`
	TaskID             string `json:"task_id"`
	ProviderRevision   string `json:"provider_revision"`
	LeaseGeneration    int64  `json:"lease_generation"`
	BaseSHA            string `json:"base_sha"`
	CandidateSHA       string `json:"candidate_sha"`
	MergeSHA           string `json:"merge_sha"`
	PatchID            string `json:"patch_id"`
	AcceptanceDigest   string `json:"acceptance_digest"`
	VerificationDigest string `json:"verification_digest"`
	RiskTier           string `json:"risk_tier"`
	AuthorFamily       string `json:"author_family"`
	ReviewerFamily     string `json:"reviewer_family"`
	Verdict            string `json:"verdict"`
	IntegrationResult  string `json:"integration_result"`
	Digest             string `json:"digest"`
}

// receiptForDigest is the canonical digest pre-image: every field except
// Digest itself, in declaration order.
type receiptForDigest struct {
	Version            int    `json:"version"`
	RepoID             string `json:"repo_id"`
	TaskRef            string `json:"task_ref"`
	TaskID             string `json:"task_id"`
	ProviderRevision   string `json:"provider_revision"`
	LeaseGeneration    int64  `json:"lease_generation"`
	BaseSHA            string `json:"base_sha"`
	CandidateSHA       string `json:"candidate_sha"`
	MergeSHA           string `json:"merge_sha"`
	PatchID            string `json:"patch_id"`
	AcceptanceDigest   string `json:"acceptance_digest"`
	VerificationDigest string `json:"verification_digest"`
	RiskTier           string `json:"risk_tier"`
	AuthorFamily       string `json:"author_family"`
	ReviewerFamily     string `json:"reviewer_family"`
	Verdict            string `json:"verdict"`
	IntegrationResult  string `json:"integration_result"`
}

// ComputeDigest returns SHA-256 over the canonical JSON form of the receipt
// with Digest omitted.
func (r CompletionReceipt) ComputeDigest() string {
	b, _ := json.Marshal(receiptForDigest{
		Version: r.Version, RepoID: r.RepoID, TaskRef: r.TaskRef, TaskID: r.TaskID,
		ProviderRevision: r.ProviderRevision, LeaseGeneration: r.LeaseGeneration,
		BaseSHA: r.BaseSHA, CandidateSHA: r.CandidateSHA, MergeSHA: r.MergeSHA,
		PatchID: r.PatchID, AcceptanceDigest: r.AcceptanceDigest,
		VerificationDigest: r.VerificationDigest, RiskTier: r.RiskTier,
		AuthorFamily: r.AuthorFamily, ReviewerFamily: r.ReviewerFamily,
		Verdict: r.Verdict, IntegrationResult: r.IntegrationResult,
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

	ref = NormalizeRef(ref)
	if !strings.EqualFold(NormalizeRef(r.TaskRef), ref) {
		return fmt.Errorf("receipt is bound to %s, not %s", r.TaskRef, ref)
	}
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
		if strings.TrimSpace(f.val) == "" {
			return fmt.Errorf("receipt is missing %s", f.name)
		}
	}
	if r.LeaseGeneration <= 0 {
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

	// Content binding: the merged commit must carry the accepted candidate's
	// patch. This is what makes "an empty commit naming the ticket" useless —
	// an empty commit has no patch id at all.
	landed, err := PatchID(repoDir, r.MergeSHA)
	if err != nil {
		return fmt.Errorf("merge sha %s: %w", r.MergeSHA, err)
	}
	if landed != r.PatchID {
		return fmt.Errorf("merge sha %s carries patch %s, not the accepted candidate's patch %s",
			r.MergeSHA, landed, r.PatchID)
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

// WriteReceipt seals and durably writes a receipt for ref. Producers use this;
// it is the seam the integration pipeline writes through.
func WriteReceipt(repoDir string, r *CompletionReceipt) error {
	r.Seal()
	path := ReceiptPath(repoDir, r.TaskRef)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("receipt dir: %w", err)
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("encode receipt: %w", err)
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
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
