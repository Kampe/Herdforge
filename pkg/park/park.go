package park

import (
	"context"
	"fmt"
	"os"
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
}

var (
	ErrNotCommit  = fmt.Errorf("is not a commit")
	ErrPushFailed = fmt.Errorf("push failed")
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

func tagExists(ctx context.Context, repoRoot, tag string) bool {
	cmd := execCommandContext(ctx, "git", "tag", "-l", tag)
	cmd.Dir = repoRoot
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == tag
}

func Park(ctx context.Context, opts ParkOptions, slug, sha, msg string) (*ParkResult, error) {
	if strings.TrimSpace(msg) == "" {
		return nil, fmt.Errorf("herd-park: -m <message> is required. Say what is DONE, what is NOT, and the exact next step; a resume note that lives only in chat is gone by the next session.")
	}

	commitSHA, err := verifyCommit(ctx, opts.RepoRoot, sha)
	if err != nil {
		return nil, fmt.Errorf("herd-park: '%s' %w", sha, err)
	}
	fullCommitSHA := commitSHA

	tag := fmt.Sprintf("parked/%s", strings.TrimPrefix(slug, "parked/"))

	var signed bool
	var signerStderr string

	// try signed first
	signCmd := execCommandContext(ctx, "git", "tag", "-f", "-m", msg, tag, fullCommitSHA)
	signCmd.Dir = opts.RepoRoot
	if signOut, signErr := signCmd.CombinedOutput(); signErr != nil {
		signerStderr = string(signOut)

		// fallback: unsigned
		unsignCmd := execCommandContext(ctx, "git", "-c", "tag.gpgSign=false", "tag", "-f", "-m", msg, tag, fullCommitSHA)
		unsignCmd.Dir = opts.RepoRoot
		if _, unsignErr := unsignCmd.CombinedOutput(); unsignErr != nil {
			return nil, fmt.Errorf("herd-park: both signed and unsigned tag creation failed\nsigned error: %v\nsigned output: %s\nunsigned error: %v", signErr, string(signOut), unsignErr)
		}
		signed = false

		lines := strings.SplitN(signerStderr, "\n", 3)
		warnLines := strings.Join(lines[:2], "\n")
		fmt.Fprintf(os.Stderr, "herd-park: WARN could not SIGN %s, created it UNSIGNED so the park is not lost.\n", tag)
		fmt.Fprintf(os.Stderr, "  signer said: %s\n", warnLines)
		fmt.Fprintf(os.Stderr, "  Re-sign once the agent is back: git tag -f -a -m '%s' %s %s && git push --force origin refs/tags/%s\n", msg, tag, fullCommitSHA, tag)
	} else {
		_ = signOut
		signed = true
	}

	// Verify artifact: tag^{commit} must match the original commit SHA
	tagCommit, err := revParse(ctx, opts.RepoRoot, "refs/tags/"+tag+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("herd-park: cannot verify tag target: %w", err)
	}
	if tagCommit != fullCommitSHA {
		return nil, fmt.Errorf("herd-park: %s does not point at %s after creation; refusing to claim a park", tag, sha)
	}

	// Force-push
	pushCmd := execCommandContext(ctx, "git", "push", "-q", "--force", "origin", "refs/tags/"+tag)
	pushCmd.Dir = opts.RepoRoot
	if pushOut, pushErr := pushCmd.CombinedOutput(); pushErr != nil {
		short := fullCommitSHA
		if len(short) > 7 {
			short = short[:7]
		}
		fmt.Fprintf(os.Stderr, "herd-park: WARN %s created locally but the PUSH FAILED; it is not durable yet.\n  Retry: git push --force origin refs/tags/%s\n", tag, tag)
		fmt.Fprintf(os.Stderr, "  push error: %v\n  output: %s\n", pushErr, string(pushOut))
		return &ParkResult{Tag: tag, ShortSHA: short, Signed: signed}, ErrPushFailed
	}

	short := fullCommitSHA
	if len(short) > 7 {
		short = short[:7]
	}

	suffix := ""
	if !signed {
		suffix = ", but UNSIGNED (see the warning above)"
	}
	fmt.Fprintf(os.Stderr, "herd-park: %s -> %s parked DURABLY (annotated tag pushed to origin).%s\n", tag, short, suffix)

	return &ParkResult{Tag: tag, ShortSHA: short, Signed: signed}, nil
}
