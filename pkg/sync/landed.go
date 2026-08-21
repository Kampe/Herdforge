package sync

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

// FAC-565: an already-merged candidate could not be closed.
//
// A verdict admitted BEFORE the merge carries no merge_sha, so Route B's
// merge-disposition check failed on work that provably landed. The only
// available alternative was minting a sealed completion receipt after the fact,
// which requires pre-merge provenance -- lease, patch id, family bindings --
// and inventing those retrospectively is the same post-hoc provenance problem
// Route B exists to avoid.
//
// A LandedDisposition is deliberately NOT a completion receipt. It records one
// narrow, git-verifiable OBSERVATION made after the merge: this candidate is
// contained in origin/main, and here is the merge that carried it. It asserts
// nothing about acceptance, review, or authority, so it cannot be mistaken for
// the receipt it does not replace.
type LandedDisposition struct {
	Version int `json:"version"`

	Ref          string `json:"ref"`
	CandidateSHA string `json:"candidate_sha"`
	MergeSHA     string `json:"merge_sha"`
	Branch       string `json:"branch,omitempty"`

	// Method names how landing was established, so a reader never has to guess
	// whether ancestry or patch identity was used.
	Method string `json:"method"`

	// Actor and ObservedAt attribute the observation. This is who ran the
	// check, not an authorization: the disposition grants nothing by itself.
	Actor      string `json:"actor"`
	ObservedAt string `json:"observed_at"`

	Note string `json:"note"`
}

// Landing methods.
const (
	LandedByRebaseEmptyDiff = "rebase-empty-diff"
	LandedByPatchIdentity   = "patch-identity"
	LandedByAncestry        = "ancestry"
)

const landedNote = "Post-merge OBSERVATION that this candidate is contained in origin/main. " +
	"NOT a completion receipt: it proves nothing about acceptance, review, or authority, and must " +
	"never be read as one."

// LandedPath is the durable location for a ref's landed disposition.
func LandedPath(repoDir, ref string) string {
	if strings.TrimSpace(repoDir) == "" {
		repoDir = "."
	}
	return filepath.Join(repoDir, ".herd", "landed", NormalizeRef(ref)+".json")
}

// WriteLandedDisposition records an observed landing. It refuses an incomplete
// record: a disposition missing its ref, candidate, merge, or method could be
// mistaken for evidence about a different object.
func WriteLandedDisposition(repoDir string, d LandedDisposition) (*LandedDisposition, error) {
	d.Version = 1
	d.Ref = NormalizeRef(d.Ref)
	d.CandidateSHA = strings.TrimSpace(d.CandidateSHA)
	d.MergeSHA = strings.TrimSpace(d.MergeSHA)
	d.Method = strings.TrimSpace(d.Method)
	for _, f := range []struct{ name, val string }{
		{"ref", d.Ref}, {"candidate sha", d.CandidateSHA},
		{"merge sha", d.MergeSHA}, {"method", d.Method},
	} {
		if f.val == "" {
			return nil, fmt.Errorf("landed disposition requires %s", f.name)
		}
	}
	if d.Actor == "" {
		d.Actor = observingActor()
	}
	if d.ObservedAt == "" {
		d.ObservedAt = time.Now().UTC().Format(time.RFC3339)
	}
	d.Note = landedNote

	path := LandedPath(repoDir, d.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("landed disposition dir: %w", err)
	}
	blob, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode landed disposition: %w", err)
	}
	// Install atomically: a truncated disposition would bind a ref to a partial
	// candidate identity.
	tmp, err := os.CreateTemp(filepath.Dir(path), ".landed-*")
	if err != nil {
		return nil, fmt.Errorf("landed disposition temp: %w", err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(blob, '\n')); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return nil, fmt.Errorf("write landed disposition: %w", err)
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("close landed disposition: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return nil, fmt.Errorf("install landed disposition: %w", err)
	}
	return &d, nil
}

// ReadLandedDisposition loads a ref's landed disposition. A missing file is not
// an error: it means no landing has been observed.
func ReadLandedDisposition(repoDir, ref string) (*LandedDisposition, error) {
	data, err := os.ReadFile(LandedPath(repoDir, ref))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read landed disposition: %w", err)
	}
	var d LandedDisposition
	if err := json.Unmarshal(data, &d); err != nil {
		// Fail closed: a corrupt disposition must not silently read as absent
		// and let some other weaker path authorize the close.
		return nil, fmt.Errorf("parse landed disposition for %s: %w", ref, err)
	}
	return &d, nil
}

// BindsCandidate reports whether this disposition is about the given candidate.
// A disposition for a different object must never satisfy a merge check.
func (d *LandedDisposition) BindsCandidate(candidateSHA string) bool {
	if d == nil {
		return false
	}
	want := strings.TrimSpace(candidateSHA)
	if want == "" || d.CandidateSHA == "" {
		return false
	}
	if d.CandidateSHA == want {
		return true
	}
	// Either side may be abbreviated; require a real prefix, not a coincidence.
	const minPrefix = 7
	if len(want) >= minPrefix && strings.HasPrefix(d.CandidateSHA, want) {
		return true
	}
	if len(d.CandidateSHA) >= minPrefix && strings.HasPrefix(want, d.CandidateSHA) {
		return true
	}
	return false
}

func observingActor() string {
	if v := strings.TrimSpace(os.Getenv("HERD_ACTOR")); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil && strings.TrimSpace(u.Username) != "" {
		return u.Username
	}
	return "unknown"
}
