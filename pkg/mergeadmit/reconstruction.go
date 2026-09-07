package mergeadmit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harvest"
	"github.com/Kampe/Herdforge/pkg/reviewledger"
)

// ReconstructionDigest pins the complete retained attestation, including identities.
func ReconstructionDigest(row reviewledger.LedgerRow) string {
	b, _ := json.Marshal(row)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (g *Gate) reconstructionContent(req Request) (string, string, error) {
	r := req.Reconstruction
	if r == nil {
		return req.BaseSHA, req.CandidateSHA, nil
	}
	if r.SHA == "" || r.BaseSHA == "" || r.AttestationDigest == "" {
		return "", "", fmt.Errorf("reconstruction requires exact content SHA, base and attestation digest")
	}
	for _, sha := range []string{r.SHA, r.BaseSHA, req.CandidateSHA, req.BaseSHA} {
		b, err := hex.DecodeString(sha)
		if err != nil || len(b) != 20 {
			return "", "", fmt.Errorf("reconstruction requires full immutable commit IDs")
		}
	}
	rows, err := g.Ledger.AllRows()
	if err != nil {
		return "", "", err
	}
	matched := false
	for _, row := range rows {
		if row.Event == string(reviewledger.EventReconstruction) && row.SHA == r.SHA && row.CandidateSHA == req.CandidateSHA && row.Status == "attested" && strings.TrimSpace(row.ContentProof) != "" && ReconstructionDigest(row) == r.AttestationDigest {
			matched = true
		}
	}
	if !matched {
		return "", "", fmt.Errorf("missing or mismatched reconstruction attestation")
	}
	git := func(args ...string) (string, error) {
		c := exec.Command("git", args...)
		c.Dir = g.RepoDir
		b, e := c.Output()
		return string(b), e
	}
	for _, pair := range [][2]string{{req.BaseSHA, req.CandidateSHA}, {r.BaseSHA, r.SHA}} {
		if !harvest.IsAncestor(context.Background(), g.RepoDir, pair[0], pair[1]) {
			return "", "", fmt.Errorf("reconstruction base is not an ancestor")
		}
	}
	paths := func(base, head string) (string, error) {
		return git("diff", "--no-ext-diff", "--no-renames", "--name-only", "-z", base, head, "--")
	}
	original, err := paths(req.BaseSHA, req.CandidateSHA)
	if err != nil {
		return "", "", err
	}
	reconstructed, err := paths(r.BaseSHA, r.SHA)
	if err != nil {
		return "", "", err
	}
	if original == "" || original != reconstructed {
		return "", "", fmt.Errorf("reconstruction changed path set differs from reviewed scope")
	}
	for _, path := range strings.Split(strings.TrimSuffix(original, "\x00"), "\x00") {
		a, err := git("ls-tree", req.CandidateSHA, "--", path)
		if err != nil {
			return "", "", err
		}
		b, err := git("ls-tree", r.SHA, "--", path)
		if err != nil {
			return "", "", err
		}
		if a == b {
			continue
		}
		if !strings.HasSuffix(path, ".md") {
			return "", "", fmt.Errorf("reconstruction altered reviewed blob: %s", path)
		}
		// Only context relocation is permitted for Markdown. Added/deleted lines
		// and file mode must remain identical; no prose assertion grants a delta.
		delta := func(base, head string) (string, error) {
			d, e := git("diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--unified=0", base, head, "--", path)
			if e != nil {
				return "", e
			}
			var lines []string
			for _, line := range strings.Split(d, "\n") {
				if strings.HasPrefix(line, "Binary files") {
					return "", fmt.Errorf("binary documentation cannot be reanchored")
				}
				if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
					continue
				}
				if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.Contains(line, "mode ") || strings.HasPrefix(line, "\\") {
					lines = append(lines, line)
				}
			}
			return strings.Join(lines, "\n"), nil
		}
		x, e := delta(req.BaseSHA, req.CandidateSHA)
		if e != nil {
			return "", "", e
		}
		y, e := delta(r.BaseSHA, r.SHA)
		if e != nil {
			return "", "", e
		}
		if x == "" || x != y {
			return "", "", fmt.Errorf("reconstruction altered reviewed documentation delta: %s", path)
		}
	}
	return r.BaseSHA, r.SHA, nil
}
