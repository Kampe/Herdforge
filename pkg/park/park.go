package park

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

var (
	reNonAlphaNum = regexp.MustCompile(`[^a-zA-Z0-9]+`)
	reMultiDash   = regexp.MustCompile(`-{2,}`)
)

var execCommandContext = exec.CommandContext

type ParkOptions struct {
	RepoRoot  string
	SignFirst bool
}

type ParkResult struct {
	Tag      string `json:"tag"`
	ShortSHA string `json:"short_sha"`
	Signed   bool   `json:"signed"`
	// SignWarning is set only when SignFirst attempted a signed tag, that
	// attempt failed, and the unsigned fallback succeeded instead. It
	// carries the signer's error plus the exact re-sign command; the
	// caller (cmd/herd) owns printing it, not this package.
	SignWarning string `json:"sign_warning,omitempty"`
}

var (
	ErrNotCommit       = fmt.Errorf("is not a commit")
	ErrPushFailed      = fmt.Errorf("push failed")
	ErrMessageRequired = fmt.Errorf("message required")
)

func Slugify(s string) string {
	return slug(s)
}

func slug(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	s = reNonAlphaNum.ReplaceAllString(s, "-")
	s = reMultiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	return strings.Trim(s, "-")
}

func revParse(ctx context.Context, repoRoot, ref string) (string, error) {
	cmd := execCommandContext(ctx, "git", "rev-parse", ref)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func verifyCommit(ctx context.Context, repoRoot, sha string) (string, error) {
	full, err := revParse(ctx, repoRoot, sha+"^{commit}")
	if err != nil {
		return "", ErrNotCommit
	}
	return full, nil
}

// createTag runs `git tag -f -m <msg> <tag> <sha>`, optionally forcing
// unsigned mode via `-c tag.gpgSign=false` ahead of it.
func createTag(ctx context.Context, repoRoot string, unsigned bool, msg, tag, sha string) ([]byte, error) {
	args := []string{"tag", "-f", "-m", msg, tag, sha}
	if unsigned {
		args = append([]string{"-c", "tag.gpgSign=false"}, args...)
	}
	cmd := execCommandContext(ctx, "git", args...)
	cmd.Dir = repoRoot
	return cmd.CombinedOutput()
}

func signWarningText(tag, sha, msg string, signerOut []byte) string {
	allLines := strings.Split(strings.TrimSpace(string(signerOut)), "\n")
	if len(allLines) > 2 {
		allLines = allLines[:2]
	}
	warnLines := strings.Join(allLines, "\n")
	return fmt.Sprintf(
		"could not SIGN %s, created it UNSIGNED so the park is not lost.\n"+
			"  signer said: %s\n"+
			"  Re-sign once the agent is back: git tag -f -a -m '%s' %s %s && git push --force origin refs/tags/%s",
		tag, warnLines, msg, tag, sha, tag)
}

func shortenSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Park makes committed-but-unfinished work durable: it creates an annotated
// tag parked/<slug> at <sha> (falling back to unsigned when the signing
// agent is dead so the park is never lost), verifies the tag artifact
// actually points at <sha>, and force-pushes it to origin. Park never
// writes to stdout/stderr itself — all user-facing output (WARN text,
// success messages, JSON) is the caller's responsibility.
func Park(ctx context.Context, opts ParkOptions, slug, sha, msg string) (*ParkResult, error) {
	if strings.TrimSpace(msg) == "" {
		return nil, fmt.Errorf("%w: -m <message> is required. Say what is DONE, what is NOT, and the exact next step; a resume note that lives only in chat is gone by the next session", ErrMessageRequired)
	}

	fullCommitSHA, err := verifyCommit(ctx, opts.RepoRoot, sha)
	if err != nil {
		return nil, fmt.Errorf("herd-park: '%s' %w", sha, err)
	}

	tag := fmt.Sprintf("parked/%s", strings.TrimPrefix(slug, "parked/"))

	var signed bool
	var signWarning string

	if opts.SignFirst {
		signOut, signErr := createTag(ctx, opts.RepoRoot, false, msg, tag, fullCommitSHA)
		if signErr == nil {
			signed = true
		} else if unsignOut, unsignErr := createTag(ctx, opts.RepoRoot, true, msg, tag, fullCommitSHA); unsignErr != nil {
			return nil, fmt.Errorf("herd-park: both signed and unsigned tag creation failed\nsigned error: %v\nsigned output: %s\nunsigned error: %v\nunsigned output: %s", signErr, string(signOut), unsignErr, string(unsignOut))
		} else {
			signWarning = signWarningText(tag, fullCommitSHA, msg, signOut)
		}
	} else if _, err := createTag(ctx, opts.RepoRoot, true, msg, tag, fullCommitSHA); err != nil {
		return nil, fmt.Errorf("herd-park: tag creation failed: %w", err)
	}

	// Verify the artifact, not the command: tag^{commit} must match the
	// exact commit SHA requested, or refuse to claim the park.
	tagCommit, err := revParse(ctx, opts.RepoRoot, "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("herd-park: cannot verify tag target: %w", err)
	}
	if tagCommit != fullCommitSHA {
		return nil, fmt.Errorf("herd-park: %s does not point at %s after creation; refusing to claim a park", tag, sha)
	}

	res := &ParkResult{Tag: tag, ShortSHA: shortenSHA(fullCommitSHA), Signed: signed, SignWarning: signWarning}

	pushCmd := execCommandContext(ctx, "git", "push", "-q", "--force", "origin", "refs/tags/"+tag)
	pushCmd.Dir = opts.RepoRoot
	if pushOut, pushErr := pushCmd.CombinedOutput(); pushErr != nil {
		return res, fmt.Errorf("%w: %s created locally but the PUSH FAILED; it is not durable yet. Retry: git push --force origin refs/tags/%s (push error: %v; output: %s)",
			ErrPushFailed, tag, tag, pushErr, strings.TrimSpace(string(pushOut)))
	}

	return res, nil
}
