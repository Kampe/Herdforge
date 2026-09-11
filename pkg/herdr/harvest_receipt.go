package herdr

// Harvest-merge staging worktrees are created by `herd harvest-merge` and are
// DELIBERATELY kept on success so the coordinator can push from them. Nothing
// recorded that they existed, so no consumer could ever prove one was
// disposable and every one of them leaked forever.
//
// This file owns the receipt that makes exactly those worktrees provably
// retirable, and nothing else. Two invariants separate it from the source-lane
// registry next door, which promises the opposite:
//
//   - Source retirement NEVER deletes a worktree. A harvest receipt exists
//     precisely to authorize deleting one, so the two authorities must not
//     share a registry. Reading one as the other is how a delete authority
//     gets acquired by accident.
//   - A receipt is NECESSARY, NEVER SUFFICIENT. It names a candidate for the
//     existing reap gates; it never substitutes for landing proof, cleanliness,
//     lock, owner, or lease checks.
//
// The stale-path problem it has to solve: a path is not an identity. Worktree
// P is harvested, receipted, and removed; a later, unrelated worktree is
// created at P. A receipt that bound only the path would authorize destroying
// that new worktree.
//
// The binding is therefore a GENERATION MARKER the producer writes into the
// registration's own private git admin directory
// (<common-git-dir>/worktrees/<ID>/), carrying a random nonce. `git worktree
// remove` and `git worktree prune` delete that directory with the
// registration, so the marker cannot survive a remove/recreate cycle and a
// re-created worktree at the same path has no marker at all.
//
// A per-registration HEAD reflog was considered for this and REJECTED: reflog
// timestamps have one-second precision, so removing and recreating the same
// path from the same base as the same actor inside one second reproduces a
// byte-identical birth line, and git reuses the basename-derived admin ID.
// TestAuthorizeHarvestRetirement_RefusesRecreationWithIdenticalReflogBirthLine
// pins that exact case. Reflog content is diagnostic here, never authority.

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/gitroot"
)

// HarvestRetirementReceiptsFile is the durable harvest receipt journal. It is
// deliberately a separate file from the source-lane retirement manifests.
const HarvestRetirementReceiptsFile = ".herd/source/harvest-receipts.jsonl"

// HarvestGenerationMarkerFile is the producer-owned generation marker, written
// inside the registration's PRIVATE git admin directory. It is not a
// repository file: it is never committed, never ignored, and never survives
// the registration it belongs to.
const HarvestGenerationMarkerFile = "herd-harvest-generation.json"

// maxHarvestMarkerBytes bounds the marker read. A marker is a fixed-shape
// record; anything larger is not one.
const maxHarvestMarkerBytes = 4096

// HarvestGenerationMarker is the live per-registration generation token.
type HarvestGenerationMarker struct {
	Version int `json:"version"`
	// Generation is a random nonce minted once, when the producer creates the
	// registration. It is the whole identity: a later registration at the same
	// path mints a different one, and a removed registration has none.
	Generation string `json:"generation"`
	// Worktree is recorded repo-relative for forensics; the marker's location
	// inside the registration is what binds it, not this field.
	Worktree  string `json:"worktree"`
	CreatedAt string `json:"created_at"`
}

// HarvestRetirementReceipt is the durable evidence that one harvest-merge
// staging worktree was created by this repository's own harvest producer, from
// an exact reviewed candidate, at an exact registration generation.
type HarvestRetirementReceipt struct {
	Repository string `json:"repository"`
	// Worktree is repo-relative, like every other lifecycle record here.
	Worktree   string `json:"worktree"`
	TempBranch string `json:"temp_branch"`
	Lane       string `json:"lane"`
	// BaseSHA, CandidateSHA and HeadSHA record the exact reviewed identity the
	// harvest carried. They are the evidence of WHAT was harvested; they are
	// deliberately NOT used as an ancestry gate, because a squash-merge lands
	// the reviewed content under a new commit and leaves none of them an
	// ancestor of main. Landing is proven against the CURRENT tip by the
	// reaper's existing whole-range content proof.
	BaseSHA      string `json:"base_sha"`
	CandidateSHA string `json:"candidate_sha"`
	HeadSHA      string `json:"head_sha"`
	// RegistrationID is the git admin directory name. It is a cheap
	// cross-check, NOT an identity: git derives it from the basename and
	// reuses it after a prune. Generation is the identity.
	RegistrationID string `json:"registration_id"`
	// Generation is the marker nonce this receipt is bound to.
	Generation    string `json:"generation"`
	BindingDigest string `json:"binding_digest"`
	RecordedAt    string `json:"recorded_at"`
}

// HarvestRetirementReceiptsPath resolves the journal under a repository root.
func HarvestRetirementReceiptsPath(root string) string {
	return filepath.Join(root, HarvestRetirementReceiptsFile)
}

// HarvestRetirementBindingDigest binds every identity field at once. The
// digest and the timestamp are excluded so the digest covers the claim, not
// itself.
func HarvestRetirementBindingDigest(r HarvestRetirementReceipt) string {
	r.BindingDigest, r.RecordedAt = "", ""
	b, _ := json.Marshal(r)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// NewHarvestRetirementReceipt stamps the time and the binding digest.
func NewHarvestRetirementReceipt(now time.Time, r HarvestRetirementReceipt) HarvestRetirementReceipt {
	if strings.TrimSpace(r.RecordedAt) == "" {
		r.RecordedAt = now.UTC().Format(time.RFC3339Nano)
	}
	r.BindingDigest = HarvestRetirementBindingDigest(r)
	return r
}

// ValidateHarvestRetirementReceipt refuses every incomplete, non-exact, or
// tampered receipt. A receipt that does not validate is not weak evidence; it
// is no evidence, and the caller must treat it as absent.
func ValidateHarvestRetirementReceipt(r HarvestRetirementReceipt) error {
	if !exactSHA(r.BaseSHA) || !exactSHA(r.CandidateSHA) || !exactSHA(r.HeadSHA) {
		return errors.New("harvest receipt requires full-length base, candidate, and head object names")
	}
	fields := []struct{ name, value string }{
		{"repository", r.Repository}, {"worktree", r.Worktree}, {"temp_branch", r.TempBranch},
		{"lane", r.Lane}, {"registration_id", r.RegistrationID}, {"generation", r.Generation},
		{"recorded_at", r.RecordedAt},
	}
	for _, f := range fields {
		if strings.TrimSpace(f.value) == "" {
			return fmt.Errorf("harvest receipt missing %s", f.name)
		}
	}
	if filepath.IsAbs(r.Worktree) {
		return errors.New("harvest receipt worktree must be repository-relative")
	}
	clean := filepath.Clean(r.Worktree)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return errors.New("harvest receipt worktree escapes the repository")
	}
	// Containment is part of the schema, not a caller's option: a receipt that
	// names anything outside the managed worktree directory is malformed.
	managed := filepath.Join(".herd", "worktrees") + string(filepath.Separator)
	if !strings.HasPrefix(clean, managed) {
		return fmt.Errorf("harvest receipt worktree %q is outside .herd/worktrees", r.Worktree)
	}
	if r.BindingDigest != HarvestRetirementBindingDigest(r) {
		return errors.New("harvest receipt binding digest is invalid")
	}
	return nil
}

// HarvestRetirementRegistry is the append-only journal of harvest receipts.
type HarvestRetirementRegistry struct{ Path string }

// Record appends one validated receipt.
func (r HarvestRetirementRegistry) Record(receipt HarvestRetirementReceipt) error {
	if err := ValidateHarvestRetirementReceipt(receipt); err != nil {
		return err
	}
	if strings.TrimSpace(r.Path) == "" {
		return errors.New("harvest retirement registry path is required")
	}
	if err := os.MkdirAll(filepath.Dir(r.Path), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(r.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// All returns every recorded receipt in journal order.
func (r HarvestRetirementRegistry) All() ([]HarvestRetirementReceipt, error) {
	f, err := os.Open(r.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []HarvestRetirementReceipt
	s := bufio.NewScanner(f)
	for s.Scan() {
		if len(strings.TrimSpace(s.Text())) == 0 {
			continue
		}
		var receipt HarvestRetirementReceipt
		if err := json.Unmarshal(s.Bytes(), &receipt); err != nil {
			return nil, fmt.Errorf("decode harvest retirement registry: %w", err)
		}
		out = append(out, receipt)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// HarvestRegistrationDir resolves a linked worktree's PRIVATE git admin
// directory. The resolution is pinned to canonical common-git-dir containment
// (FAC-565): a registration is exactly <common-git-dir>/worktrees/<ID>,
// resolved from the worktree's own repository through gitroot.CommonDir --
// never merely a git dir whose parent happens to be NAMED "worktrees". That
// name-only check accepted a main checkout whose repository sits at a path
// literally named worktrees, which let the producer mint a marker into an
// unrelated git admin namespace. The main checkout is refused: its git dir is
// not a registration and must never carry a harvest marker, whatever its
// directory layout. The authoritative proof that a surface IS this
// repository's registration remains git's own registration set (worktree
// list --porcelain, enforced by the act-time identity fence); this pin bounds
// where a marker may be minted and read on top of it.
func HarvestRegistrationDir(worktreePath string) (string, error) {
	out, err := exec.Command("git", "-C", worktreePath, "rev-parse", "--absolute-git-dir").Output()
	if err != nil {
		return "", fmt.Errorf("resolve git dir for %s: %w", worktreePath, err)
	}
	gitDir := strings.TrimSpace(string(out))
	if gitDir == "" {
		return "", fmt.Errorf("resolve git dir for %s: empty", worktreePath)
	}
	commonDir, err := gitroot.CommonDir(context.Background(), worktreePath)
	if err != nil {
		return "", fmt.Errorf("resolve common git dir for %s: %w", worktreePath, err)
	}
	// Both git answers are real-pathed absolutes; normalize both sides through
	// the one canonicalization anyway so an environment where either answer
	// arrives unresolved can never read as a containment failure.
	resolvedGitDir, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve git dir for %s: %w", worktreePath, err)
	}
	resolvedCommon, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		return "", fmt.Errorf("resolve common git dir for %s: %w", worktreePath, err)
	}
	registrationID := filepath.Base(resolvedGitDir)
	if resolvedGitDir != filepath.Join(resolvedCommon, "worktrees", registrationID) {
		return "", fmt.Errorf("%s is not a linked worktree registration of %s", worktreePath, commonDir)
	}
	return resolvedGitDir, nil
}

// MintHarvestGenerationMarker writes a fresh generation marker into a
// registration's private admin directory and returns it. The producer calls
// this once, immediately after creating the staging worktree.
func MintHarvestGenerationMarker(worktreePath, repoRelative string, now time.Time) (HarvestGenerationMarker, string, error) {
	gitDir, err := HarvestRegistrationDir(worktreePath)
	if err != nil {
		return HarvestGenerationMarker{}, "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return HarvestGenerationMarker{}, "", fmt.Errorf("mint harvest generation: %w", err)
	}
	marker := HarvestGenerationMarker{
		Version:    1,
		Generation: hex.EncodeToString(buf),
		Worktree:   repoRelative,
		CreatedAt:  now.UTC().Format(time.RFC3339Nano),
	}
	b, err := json.Marshal(marker)
	if err != nil {
		return HarvestGenerationMarker{}, "", err
	}
	path := filepath.Join(gitDir, HarvestGenerationMarkerFile)
	if err := os.WriteFile(path, append(b, '\n'), 0o600); err != nil {
		return HarvestGenerationMarker{}, "", fmt.Errorf("write harvest generation marker: %w", err)
	}
	return marker, filepath.Base(gitDir), nil
}

// ReadHarvestGenerationMarker reads the live marker for a registration. An
// absent, oversized, malformed, wrong-version, symlinked, or otherwise
// non-regular marker yields no authority.
func ReadHarvestGenerationMarker(worktreePath string) (HarvestGenerationMarker, string, error) {
	gitDir, err := HarvestRegistrationDir(worktreePath)
	if err != nil {
		return HarvestGenerationMarker{}, "", err
	}
	path := filepath.Join(gitDir, HarvestGenerationMarkerFile)
	// Lstat, not Stat: the marker's authority is the file the registration's
	// own admin directory carries. A symlink parked at the marker path names
	// some other file, so it is not a marker no matter what it points at.
	info, err := os.Lstat(path)
	if err != nil {
		return HarvestGenerationMarker{}, "", fmt.Errorf("read harvest generation marker: %w", err)
	}
	if !info.Mode().IsRegular() {
		return HarvestGenerationMarker{}, "", fmt.Errorf("read harvest generation marker: %s is not a regular file (%s), refusing marker authority", path, info.Mode().Type())
	}
	// Bounded by the READ, not by a size probe: at most limit+1 bytes are
	// consumed, and anything longer than the limit is refused, so a file
	// that grew after the stat can never be partially trusted.
	data, err := readBoundedHarvestMarker(path, maxHarvestMarkerBytes)
	if err != nil {
		return HarvestGenerationMarker{}, "", err
	}
	var marker HarvestGenerationMarker
	if err := json.Unmarshal(data, &marker); err != nil {
		return HarvestGenerationMarker{}, "", fmt.Errorf("harvest generation marker is not native schema: %w", err)
	}
	if marker.Version != 1 {
		return HarvestGenerationMarker{}, "", fmt.Errorf("harvest generation marker version %d is not supported", marker.Version)
	}
	if strings.TrimSpace(marker.Generation) == "" {
		return HarvestGenerationMarker{}, "", errors.New("harvest generation marker carries no generation")
	}
	return marker, filepath.Base(gitDir), nil
}

// readBoundedHarvestMarker consumes at most limit+1 bytes of the file and
// refuses any file longer than limit. Every error preserves its cause.
func readBoundedHarvestMarker(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read harvest generation marker: %w", err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read harvest generation marker: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("read harvest generation marker: %s is %d+ bytes, larger than a marker can be", path, limit)
	}
	return data, nil
}

// HarvestRetirementCandidates is the bulk, in-memory answer to one bounded
// scheduling question: which surfaces does the receipt journal of THIS
// repository even name. It is a NECESSARY pre-filter for callers that must
// not spend per-path subprocesses on surfaces the journal never named (the
// reap pulse), and it is NEVER authority: candidacy only admits a surface to
// a bounded inspection window, where the full act-grade binding (live
// generation marker + journal + digest) still decides.
type HarvestRetirementCandidates struct {
	surfaces map[string]bool
}

// LoadHarvestRetirementCandidates reads the receipt journal ONCE and indexes
// every valid receipt of one repository identity by canonical surface path.
// An unreadable journal is an error, never an empty index that could read as
// "no candidates". A caller that answers the error by skipping detached
// surfaces keeps the historical pool answer, which is the fail-closed
// direction.
func LoadHarvestRetirementCandidates(root, repositoryIdentity string) (*HarvestRetirementCandidates, error) {
	if strings.TrimSpace(repositoryIdentity) == "" {
		return nil, errors.New("harvest retirement candidates: repository identity is required")
	}
	all, err := (HarvestRetirementRegistry{Path: HarvestRetirementReceiptsPath(root)}).All()
	if err != nil {
		return nil, fmt.Errorf("harvest retirement candidates: %w", err)
	}
	rootIdentity := canonicalHarvestPath(root)
	index := &HarvestRetirementCandidates{surfaces: make(map[string]bool, len(all))}
	for _, receipt := range all {
		if err := ValidateHarvestRetirementReceipt(receipt); err != nil {
			// A malformed line cannot claim any surface -- the same rule the
			// authorizer applies -- so it cannot make one a candidate either.
			continue
		}
		if receipt.Repository != repositoryIdentity {
			continue
		}
		index.surfaces[canonicalHarvestPath(filepath.Join(rootIdentity, receipt.Worktree))] = true
	}
	return index, nil
}

// NamesSurface reports whether any valid receipt of the repository names the
// exact surface. Lookup is in-memory plus one local path canonicalization:
// no Git subprocess, no journal re-read, no marker read.
func (c *HarvestRetirementCandidates) NamesSurface(worktreePath string) bool {
	if c == nil || len(c.surfaces) == 0 {
		return false
	}
	return c.surfaces[canonicalHarvestPath(worktreePath)]
}

// HarvestRetirementRequest is one exact surface being considered for harvest
// retirement.
type HarvestRetirementRequest struct {
	// Root is the repository root the registry and relative paths anchor to.
	Root string
	// RepositoryIdentity is the authenticated identity of THIS repository.
	RepositoryIdentity string
	// WorktreePath is the path exactly as git registered it.
	WorktreePath string
}

// AuthorizeHarvestRetirement proves that an exact surface is the very harvest
// registration a receipt describes. It answers ONLY the ownership question:
// this repository's own harvest producer created this exact registration and
// recorded it.
//
// It deliberately proves nothing about landing, cleanliness, locks, leases, or
// owners. Those gates already exist in the reaper and still run; this returning
// nil never means "remove it".
//
// Every failure keeps the worktree. Absent, unreadable, ambiguous, and
// duplicate evidence are all refusals -- never a guess of safety.
func AuthorizeHarvestRetirement(req HarvestRetirementRequest) (HarvestRetirementReceipt, error) {
	var zero HarvestRetirementReceipt
	if strings.TrimSpace(req.RepositoryIdentity) == "" {
		return zero, errors.New("harvest retirement: repository identity is unknown, refusing")
	}
	rootIdentity := canonicalHarvestPath(req.Root)
	surface := canonicalHarvestPath(req.WorktreePath)

	registry := HarvestRetirementRegistry{Path: HarvestRetirementReceiptsPath(req.Root)}
	all, err := registry.All()
	if err != nil {
		return zero, fmt.Errorf("harvest retirement: receipt journal unreadable: %w, refusing", err)
	}

	// P1: a valid receipt of THIS repository names this exact surface.
	var matched []HarvestRetirementReceipt
	for _, receipt := range all {
		if err := ValidateHarvestRetirementReceipt(receipt); err != nil {
			// A malformed line cannot claim any surface, so it is skipped
			// rather than failing every unrelated retirement in the fleet.
			continue
		}
		if receipt.Repository != req.RepositoryIdentity {
			continue
		}
		if canonicalHarvestPath(filepath.Join(rootIdentity, receipt.Worktree)) != surface {
			continue
		}
		matched = append(matched, receipt)
	}
	if len(matched) == 0 {
		return zero, fmt.Errorf("harvest retirement: no valid receipt of this repository names %s, refusing", req.WorktreePath)
	}

	// P2: the live generation marker in this registration's private admin
	// directory must be the one the receipt was bound to. This is what makes
	// the receipt an identity rather than a path match: `git worktree remove`
	// prunes the admin directory, so a worktree later re-created at the same
	// path has no marker, and a re-harvest there mints a different one.
	marker, registrationID, err := ReadHarvestGenerationMarker(req.WorktreePath)
	if err != nil {
		return zero, fmt.Errorf("harvest retirement: %s has no readable generation marker: %v; "+
			"an unmarked registration is not the one any receipt describes, refusing", req.WorktreePath, err)
	}
	var bound []HarvestRetirementReceipt
	for _, receipt := range matched {
		if receipt.Generation == marker.Generation && receipt.RegistrationID == registrationID {
			bound = append(bound, receipt)
		}
	}

	if len(bound) == 0 {
		return zero, fmt.Errorf("harvest retirement: %s carries generation %s, which no receipt for this path is bound to "+
			"(the path was reused); a stale receipt is not authority over a later registration, refusing",
			req.WorktreePath, shortHarvestGeneration(marker.Generation))
	}
	if len(bound) > 1 {
		return zero, fmt.Errorf("harvest retirement: %s is named by %d receipts bound to one generation; ambiguous evidence, refusing", req.WorktreePath, len(bound))
	}
	return bound[0], nil
}

func shortHarvestGeneration(generation string) string {
	if len(generation) > 12 {
		return generation[:12]
	}
	return generation
}

// canonicalHarvestPath resolves a path to one absolute, symlink-resolved,
// cleaned identity so a spelling never decides a retirement.
func canonicalHarvestPath(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = filepath.Clean(path)
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	return filepath.Clean(abs)
}
