package verifier

import (
	"bytes"
	"context"
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
	"sync"
	"syscall"
	"time"
)

type Language string

const (
	LangGo      Language = "go"
	LangNode    Language = "node"
	LangPython  Language = "python"
	LangRust    Language = "rust"
	LangUnknown Language = "unknown"
)

type Outcome string

const (
	OutcomePASS    Outcome = "PASS"
	OutcomeFAIL    Outcome = "FAIL"
	OutcomeBLOCKED Outcome = "BLOCKED"
)

// EnvironmentPolicy is explicit because a receipt must not call an ambient
// process environment hermetic. Inherited is honest about using the caller's
// environment; Hermetic uses the fixed allowlist in hermeticEnvironment.
type EnvironmentPolicy string

const (
	EnvironmentPolicyInherited EnvironmentPolicy = "inherited"
	EnvironmentPolicyHermetic  EnvironmentPolicy = "hermetic"
)

const MaxRetainedOutputBytes = 1 << 20

type Result struct {
	Passed       bool
	Outcome      Outcome
	Output       string
	OutputDigest string
	ExitCode     int
	Duration     time.Duration
}

// MutationResult describes both the negative run and the restoration gate.
// Killed is true only when the applied mutant made the candidate's tests fail
// and the original candidate passed again after restoration.
type MutationResult struct {
	MutantID     string
	Killed       bool
	Outcome      Outcome
	Output       string
	CandidateSHA string
	Restored     bool
	Baseline     Receipt
	Mutant       Receipt
	Final        Receipt
}

// VerificationRequest identifies the immutable candidate a receipt belongs
// to. CandidateSHA must be the full 40-character commit SHA. BaseSHA is
// optional, but when present it has the same exact-SHA requirement.
type VerificationRequest struct {
	TaskRef           string
	LeaseGeneration   string
	CandidateSHA      string
	BaseSHA           string
	EnvironmentPolicy EnvironmentPolicy
	Artifacts         []string
}

// Receipt is durable evidence for one command run against one candidate.
// Digest is SHA-256 over the canonical JSON form of the receipt with Digest
// omitted. Output is represented by OutputDigest so receipts stay bounded.
type Receipt struct {
	Version           int               `json:"version"`
	TaskRef           string            `json:"task_ref"`
	LeaseGeneration   string            `json:"lease_generation"`
	CandidateSHA      string            `json:"candidate_sha"`
	BaseSHA           string            `json:"base_sha,omitempty"`
	Command           []string          `json:"argv"`
	ExitCode          int               `json:"exit_code"`
	Duration          time.Duration     `json:"duration_ns"`
	EnvironmentPolicy EnvironmentPolicy `json:"environment_policy"`
	Artifacts         []string          `json:"artifacts,omitempty"`
	OutputDigest      string            `json:"output_digest,omitempty"`
	Outcome           Outcome           `json:"outcome"`
	Digest            string            `json:"digest"`
}

type receiptForDigest struct {
	Version           int               `json:"version"`
	TaskRef           string            `json:"task_ref"`
	LeaseGeneration   string            `json:"lease_generation"`
	CandidateSHA      string            `json:"candidate_sha"`
	BaseSHA           string            `json:"base_sha,omitempty"`
	Command           []string          `json:"argv"`
	ExitCode          int               `json:"exit_code"`
	Duration          time.Duration     `json:"duration_ns"`
	EnvironmentPolicy EnvironmentPolicy `json:"environment_policy"`
	Artifacts         []string          `json:"artifacts,omitempty"`
	OutputDigest      string            `json:"output_digest,omitempty"`
	Outcome           Outcome           `json:"outcome"`
}

type Verifier struct {
	Argv       []string
	commandErr error
	Timeout    time.Duration
}

// NewVerifier preserves the existing config-string entry point, but parses a
// shell-like argv syntax without invoking a shell. Quoting and backslashes are
// interpreted only to construct argv; no expansion, pipes, redirects, or
// command substitution can occur.
func NewVerifier(command string) *Verifier {
	argv, err := parseArgv(command)
	return &Verifier{Argv: argv, commandErr: err}
}

// NewVerifierArgs is the preferred constructor. The caller supplies the exact
// argv passed to exec.CommandContext, so whitespace inside an argument is
// preserved and never reparsed.
func NewVerifierArgs(argv []string) *Verifier {
	copyArgv := append([]string(nil), argv...)
	var err error
	if len(copyArgv) == 0 || strings.TrimSpace(copyArgv[0]) == "" {
		err = errors.New("verification command is empty")
	}
	return &Verifier{Argv: copyArgv, commandErr: err}
}

func (v *Verifier) Execute(ctx context.Context, dir string) (*Result, error) {
	return v.execute(ctx, dir, EnvironmentPolicyInherited)
}

func (v *Verifier) execute(ctx context.Context, dir string, policy EnvironmentPolicy) (*Result, error) {
	if v == nil {
		return nil, errors.New("nil verifier")
	}
	if v.commandErr != nil {
		return nil, v.commandErr
	}
	if len(v.Argv) == 0 {
		return nil, errors.New("verification command is empty")
	}
	if err := validateEnvironmentPolicy(policy); err != nil {
		return nil, err
	}

	started := time.Now()
	commandPath := v.Argv[0]
	var commandEnv []string
	if policy == EnvironmentPolicyHermetic {
		commandEnv = hermeticEnvironment()
		var err error
		commandPath, err = resolveHermeticExecutable(commandPath, environmentValue(commandEnv, "PATH"))
		if err != nil {
			output := []byte(err.Error())
			return &Result{
				Outcome:      OutcomeBLOCKED,
				Output:       boundedOutput(output),
				OutputDigest: digestBytes(output),
				ExitCode:     -1,
				Duration:     time.Since(started),
			}, nil
		}
	}
	cmd := exec.CommandContext(ctx, commandPath, v.Argv[1:]...)
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Cancel reaps the whole process group while the leader is still live.
	// Group-wide kill is required: leader-only kill leaves shell grandchildren
	// running (see TestExecuteCancellationRequiresProcessGroupReap).
	// No post-Wait kill: that re-introduces PID-reuse hazard; Cancel is enough.
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return processGroupKiller(cmd.Process.Pid)
	}
	if policy == EnvironmentPolicyHermetic {
		cmd.Env = commandEnv
	}
	// A canceled shell can leave grandchildren holding stdout/stderr pipes
	// open (for example, `sh -c 'sleep 3'`). WaitDelay bounds that wait and
	// keeps the mutation transaction's restoration defer reachable.
	cmd.WaitDelay = 100 * time.Millisecond

	// concurrentCombinedWriter serializes Writes from the stdout and stderr
	// pipe-copy goroutines. A bare bytes.Buffer races under -race when both
	// streams are attached (same shape as exec.CombinedOutput, but locked).
	var combined concurrentCombinedWriter
	cmd.Stdout = &combined
	cmd.Stderr = &combined
	if err := cmd.Start(); err != nil {
		// Cancellation (and other Start failures) must surface as BLOCKED
		// Result, not a bare error — RunMutationCheckForCandidate relies on
		// (result, nil) so the restore defer still records Restored evidence.
		output := []byte(err.Error())
		return &Result{
			Outcome:      OutcomeBLOCKED,
			Output:       boundedOutput(output),
			OutputDigest: digestBytes(output),
			ExitCode:     -1,
			Duration:     time.Since(started),
		}, nil
	}
	waitErr := cmd.Wait()
	output := combined.bytes()
	err := waitErr
	result := &Result{
		Passed:       err == nil,
		Outcome:      OutcomePASS,
		Output:       boundedOutput(output),
		OutputDigest: digestBytes(output),
		ExitCode:     exitCode(cmd, err),
		Duration:     time.Since(started),
	}
	if err != nil {
		result.Outcome = OutcomeFAIL
		if ctx.Err() != nil || cmd.ProcessState == nil {
			result.Outcome = OutcomeBLOCKED
		}
		result.Output = boundedOutput([]byte(fmt.Sprintf("verification failed: %v\noutput:\n%s", err, string(output))))
	}
	return result, nil
}

// concurrentCombinedWriter is an io.Writer used for both cmd.Stdout and
// cmd.Stderr. exec starts one copy goroutine per pipe; without the mutex,
// -race reports concurrent writes into bytes.Buffer.
type concurrentCombinedWriter struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (w *concurrentCombinedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.b.Write(p)
}

func (w *concurrentCombinedWriter) bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	// Copy so callers can retain the slice after the next Write.
	out := make([]byte, w.b.Len())
	copy(out, w.b.Bytes())
	return out
}

// VerifyCandidate runs the configured argv only when dir is a clean checkout
// pinned to req.CandidateSHA. A command failure is FAIL. A dirty checkout,
// SHA mismatch, malformed execution, timeout, or cancellation is BLOCKED.
func (v *Verifier) VerifyCandidate(ctx context.Context, dir string, req VerificationRequest) (*Receipt, error) {
	if v == nil {
		return nil, errors.New("nil verifier")
	}
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	if err := requireCleanCandidate(dir, req.CandidateSHA); err != nil {
		return blockedReceipt(req, v.argv(), 0, nil, err), nil
	}

	result, err := v.execute(ctx, dir, req.EnvironmentPolicy)
	if err != nil {
		return nil, err
	}
	outcome := result.Outcome
	if err := requireCleanCandidate(dir, req.CandidateSHA); err != nil {
		outcome = OutcomeBLOCKED
		result.Output += "\ncandidate changed during verification: " + err.Error()
	}
	receipt := makeReceipt(req, v.argv(), result, outcome)
	return &receipt, nil
}

// ValidateReceipt checks the receipt digest, exact candidate SHA, and clean
// checkout. This is the verifier-side admission primitive; callers must run it
// against the current candidate before accepting the receipt.
func (r Receipt) ValidateReceipt(ctx context.Context, dir string) error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported receipt version %d", r.Version)
	}
	if !validSHA(r.CandidateSHA) {
		return errors.New("receipt candidate SHA is not exact")
	}
	if err := r.ValidateDigest(); err != nil {
		return err
	}
	if err := requireCleanCandidate(dir, r.CandidateSHA); err != nil {
		return fmt.Errorf("receipt candidate is no longer current: %w", err)
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// ValidateDigest checks only the receipt's self-authenticating payload. It is
// separate from candidate validation so every signed field can be tested
// without a SHA mismatch masking a digest-coverage regression.
func (r Receipt) ValidateDigest() error {
	if r.Digest == "" || r.Digest != digestReceipt(r) {
		return errors.New("receipt digest is invalid")
	}
	return nil
}

// DetectLanguage inspects file extensions to determine testing toolchain.
func DetectLanguage(filePath string) Language {
	switch {
	case strings.HasSuffix(filePath, ".go"):
		return LangGo
	case strings.HasSuffix(filePath, ".ts"), strings.HasSuffix(filePath, ".js"), strings.HasSuffix(filePath, ".tsx"), strings.HasSuffix(filePath, ".jsx"):
		return LangNode
	case strings.HasSuffix(filePath, ".py"):
		return LangPython
	case strings.HasSuffix(filePath, ".rs"):
		return LangRust
	default:
		return LangUnknown
	}
}

func (v *Verifier) argv() []string {
	if v == nil {
		return nil
	}
	return append([]string(nil), v.Argv...)
}

func validateRequest(req VerificationRequest) error {
	if !validSHA(req.CandidateSHA) {
		return errors.New("candidate SHA must be the exact 40-character commit SHA")
	}
	if req.BaseSHA != "" && !validSHA(req.BaseSHA) {
		return errors.New("base SHA must be the exact 40-character commit SHA")
	}
	if err := validateEnvironmentPolicy(req.EnvironmentPolicy); err != nil {
		return err
	}
	return nil
}

func validateEnvironmentPolicy(policy EnvironmentPolicy) error {
	switch policy {
	case EnvironmentPolicyInherited, EnvironmentPolicyHermetic:
		return nil
	default:
		return fmt.Errorf("unsupported environment policy %q", policy)
	}
}

func hermeticEnvironment() []string {
	return []string{
		"PATH=" + hermeticPathValue,
		"LC_ALL=C",
		"LANG=C",
		"TZ=UTC",
	}
}

const hermeticPathValue = "/opt/homebrew/bin:/usr/local/go/bin:/go/bin:/usr/local/bin:/usr/bin:/bin"

func environmentValue(env []string, name string) string {
	prefix := name + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

func resolveHermeticExecutable(file, pathValue string) (string, error) {
	if filepath.IsAbs(file) || strings.ContainsRune(file, filepath.Separator) {
		return file, nil
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, file)
		info, err := os.Stat(candidate)
		if err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("executable %q not found in hermetic PATH", file)
}

func boundedOutput(output []byte) string {
	if len(output) <= MaxRetainedOutputBytes {
		return string(output)
	}
	const marker = "\n[output truncated]"
	limit := MaxRetainedOutputBytes - len(marker)
	return string(output[:limit]) + marker
}

func validSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

func currentSHA(dir string) (string, error) {
	out, err := runGit(dir, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return "", fmt.Errorf("read candidate SHA: %w", err)
	}
	sha := strings.TrimSpace(string(out))
	if !validSHA(sha) {
		return "", fmt.Errorf("git returned non-exact candidate SHA %q", sha)
	}
	return sha, nil
}

func requireCleanCandidate(dir, expectedSHA string) error {
	actual, err := currentSHA(dir)
	if err != nil {
		return err
	}
	if actual != expectedSHA {
		return fmt.Errorf("candidate SHA %s does not match expected %s", actual, expectedSHA)
	}
	out, err := runGit(dir, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return fmt.Errorf("read candidate status: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("candidate worktree is dirty: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func makeReceipt(req VerificationRequest, argv []string, result *Result, outcome Outcome) Receipt {
	outputDigest := result.OutputDigest
	if outputDigest == "" {
		outputDigest = digestBytes([]byte(result.Output))
	}
	receipt := Receipt{
		Version:           1,
		TaskRef:           req.TaskRef,
		LeaseGeneration:   req.LeaseGeneration,
		CandidateSHA:      req.CandidateSHA,
		BaseSHA:           req.BaseSHA,
		Command:           append([]string(nil), argv...),
		ExitCode:          result.ExitCode,
		Duration:          result.Duration,
		EnvironmentPolicy: req.EnvironmentPolicy,
		Artifacts:         append([]string(nil), req.Artifacts...),
		OutputDigest:      outputDigest,
		Outcome:           outcome,
	}
	receipt.Digest = digestReceipt(receipt)
	return receipt
}

func blockedReceipt(req VerificationRequest, argv []string, exitCode int, output []byte, cause error) *Receipt {
	result := &Result{Outcome: OutcomeBLOCKED, ExitCode: exitCode, Output: cause.Error()}
	if output != nil {
		result.Output += "\n" + string(output)
	}
	// OutputDigest always describes the complete output retained for this
	// synthetic blocked result. Process results instead provide the digest of
	// the complete raw process output before Result.Output is bounded.
	result.Output = boundedOutput([]byte(result.Output))
	result.OutputDigest = digestBytes([]byte(result.Output))
	receipt := makeReceipt(req, argv, result, OutcomeBLOCKED)
	return &receipt
}

func digestReceipt(receipt Receipt) string {
	payload := receiptForDigest{
		Version:           receipt.Version,
		TaskRef:           receipt.TaskRef,
		LeaseGeneration:   receipt.LeaseGeneration,
		CandidateSHA:      receipt.CandidateSHA,
		BaseSHA:           receipt.BaseSHA,
		Command:           append([]string(nil), receipt.Command...),
		ExitCode:          receipt.ExitCode,
		Duration:          receipt.Duration,
		EnvironmentPolicy: receipt.EnvironmentPolicy,
		Artifacts:         append([]string(nil), receipt.Artifacts...),
		OutputDigest:      receipt.OutputDigest,
		Outcome:           receipt.Outcome,
	}
	data, _ := json.Marshal(payload)
	return "sha256:" + digestBytes(data)
}

func digestBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func exitCode(cmd *exec.Cmd, err error) int {
	if cmd.ProcessState != nil {
		return cmd.ProcessState.ExitCode()
	}
	if err != nil {
		return -1
	}
	return 0
}

// parseArgv is deliberately a small, non-shell tokenizer. It supports the
// quoting needed by existing config strings while rejecting malformed input.
func parseArgv(command string) ([]string, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("verification command is empty")
	}
	var argv []string
	var arg strings.Builder
	started := false
	var quote rune
	escaped := false
	for _, ch := range command {
		if escaped {
			arg.WriteRune(ch)
			started = true
			escaped = false
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
				started = true
			} else if ch == '\\' && quote == '"' {
				escaped = true
			} else {
				arg.WriteRune(ch)
			}
			continue
		}
		switch ch {
		case '\\':
			escaped = true
			started = true
		case '\'', '"':
			quote = ch
			started = true
		case ' ', '\t', '\n', '\r':
			if started {
				argv = append(argv, arg.String())
				arg.Reset()
				started = false
			}
		default:
			arg.WriteRune(ch)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("verification command ends with an escape")
	}
	if quote != 0 {
		return nil, errors.New("verification command has an unterminated quote")
	}
	if started {
		argv = append(argv, arg.String())
	}
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return nil, errors.New("verification command is empty")
	}
	return argv, nil
}

func safeRelativePath(root, name string) (string, error) {
	if name == "" || filepath.IsAbs(name) {
		return "", errors.New("mutation target must be a non-empty relative path")
	}
	clean := filepath.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("mutation target escapes candidate: %q", name)
	}
	parts := strings.Split(clean, string(filepath.Separator))
	if len(parts) > 0 && parts[0] == ".git" {
		return "", errors.New("mutation target may not enter .git")
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve candidate root: %w", err)
	}
	target := filepath.Join(resolvedRoot, clean)
	info, err := os.Lstat(target)
	if err != nil {
		return "", fmt.Errorf("inspect mutation target: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", errors.New("mutation target must be an Lstat regular file")
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", fmt.Errorf("resolve mutation target: %w", err)
	}
	if !pathWithin(resolvedRoot, resolvedTarget) {
		return "", errors.New("mutation target resolves outside candidate root")
	}
	gitDir, err := resolvedGitDir(resolvedRoot)
	if err != nil {
		return "", err
	}
	if pathWithin(gitDir, resolvedTarget) {
		return "", errors.New("mutation target resolves into git metadata")
	}
	return resolvedTarget, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && rel != "." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

func resolvedGitDir(root string) (string, error) {
	out, err := runGit(root, "rev-parse", "--git-dir")
	if err != nil {
		return "", fmt.Errorf("resolve git directory: %w", err)
	}
	gitDir := strings.TrimSpace(string(out))
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	resolved, err := filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", fmt.Errorf("resolve git directory path: %w", err)
	}
	return resolved, nil
}

// MutationRequest describes one bounded, reversible mutant application.
type MutationRequest struct {
	TaskRef           string
	LeaseGeneration   string
	CandidateSHA      string
	BaseSHA           string
	EnvironmentPolicy EnvironmentPolicy
	Artifacts         []string
	TargetFile        string
	OriginalCode      string
	MutantCode        string
	Timeout           time.Duration
}

// RunMutationCheck is the compatibility entry point for callers that already
// have a target and replacement. It resolves the exact current candidate SHA
// and delegates to RunMutationCheckForCandidate.
func (v *Verifier) RunMutationCheck(ctx context.Context, dir string, targetFile string, originalCode string, mutantCode string) (*MutationResult, error) {
	if v == nil {
		return nil, errors.New("nil verifier")
	}
	sha, err := currentSHA(dir)
	if err != nil {
		return nil, err
	}
	return v.RunMutationCheckForCandidate(ctx, dir, MutationRequest{
		CandidateSHA: sha,
		TargetFile:   targetFile,
		OriginalCode: originalCode,
		MutantCode:   mutantCode,
		Timeout:      v.mutationTimeout(),
	})
}

// RunMutationCheckForCandidate proves non-vacuity with a real file mutation:
// baseline PASS, apply the supplied mutant, bounded negative run, restore the
// original bytes in a defer, and PASS again after restoration. Cancellation,
// timeout, dirty state, stale SHA, tooling errors, and restoration failures
// are BLOCKED. A mutant that passes is FAIL.
//
// On every return path the owned subprocess process-groups are reaped before
// the function returns so callers (including tests using t.TempDir) never
// race a late writer under dir/.git during cleanup.
func (v *Verifier) RunMutationCheckForCandidate(ctx context.Context, dir string, req MutationRequest) (*MutationResult, error) {
	if v == nil {
		return nil, errors.New("nil verifier")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Each owned subprocess reaps its own process group after Wait. Path-guard
	// failures never start Execute; successful mutations still return only
	// after baseline/mutant/final commands and git inspections have reaped.
	if err := validateRequest(VerificationRequest{
		CandidateSHA:      req.CandidateSHA,
		BaseSHA:           req.BaseSHA,
		TaskRef:           req.TaskRef,
		LeaseGeneration:   req.LeaseGeneration,
		EnvironmentPolicy: req.EnvironmentPolicy,
		Artifacts:         req.Artifacts,
	}); err != nil {
		return nil, err
	}
	target, err := safeRelativePath(dir, req.TargetFile)
	if err != nil {
		return nil, err
	}
	if req.Timeout <= 0 {
		req.Timeout = v.mutationTimeout()
	}
	if req.Timeout <= 0 {
		return nil, errors.New("mutation timeout must be positive")
	}

	baseReq := VerificationRequest{
		TaskRef:           req.TaskRef,
		LeaseGeneration:   req.LeaseGeneration,
		CandidateSHA:      req.CandidateSHA,
		BaseSHA:           req.BaseSHA,
		EnvironmentPolicy: req.EnvironmentPolicy,
		Artifacts:         req.Artifacts,
	}
	result := &MutationResult{
		MutantID:     "mutant-" + req.TargetFile,
		CandidateSHA: req.CandidateSHA,
		Outcome:      OutcomeBLOCKED,
	}
	if err := requireCleanCandidate(dir, req.CandidateSHA); err != nil {
		result.Output = err.Error()
		result.Baseline = *blockedReceipt(baseReq, v.argv(), 0, nil, err)
		return result, nil
	}
	if ctx.Err() != nil {
		result.Output = ctx.Err().Error()
		result.Baseline = *blockedReceipt(baseReq, v.argv(), 0, nil, ctx.Err())
		return result, nil
	}

	baseline, err := v.VerifyCandidate(ctx, dir, baseReq)
	if err != nil {
		return nil, err
	}
	result.Baseline = *baseline
	if baseline.Outcome != OutcomePASS {
		result.Outcome = baseline.Outcome
		result.Output = "baseline candidate did not PASS"
		return result, nil
	}

	original, info, err := readRegularFile(target)
	if err != nil {
		result.Output = fmt.Sprintf("read mutation target: %v", err)
		result.Mutant = *blockedReceipt(baseReq, v.argv(), 0, nil, err)
		return result, nil
	}
	if string(original) != req.OriginalCode {
		err := errors.New("mutation target does not match the supplied original candidate bytes")
		result.Output = err.Error()
		result.Mutant = *blockedReceipt(baseReq, v.argv(), 0, nil, err)
		return result, nil
	}
	defer func() {
		if restoreErr := restoreFile(target, original, info.Mode()); restoreErr == nil {
			result.Restored = true
		} else {
			result.Output += "\nrestore failed: " + restoreErr.Error()
		}
	}()
	if err := writeRegularFile(target, []byte(req.MutantCode), info.Mode()); err != nil {
		result.Output = fmt.Sprintf("apply mutant: %v", err)
		return result, nil
	}

	mutantCtx, cancel := context.WithTimeout(ctx, req.Timeout)
	mutant, execErr := v.Execute(mutantCtx, dir)
	mutantContextErr := mutantCtx.Err()
	cancel()
	if execErr != nil {
		return nil, execErr
	}
	mutantReq := baseReq
	result.Mutant = makeReceipt(mutantReq, v.argv(), mutant, mutant.Outcome)
	result.Output = mutant.Output

	if mutantContextErr != nil || ctx.Err() != nil {
		result.Outcome = OutcomeBLOCKED
		return result, nil
	}
	if mutant.Outcome == OutcomeBLOCKED {
		result.Outcome = OutcomeBLOCKED
		return result, nil
	}
	if mutant.Outcome != OutcomeFAIL {
		result.Outcome = OutcomeFAIL
		return result, nil
	}

	// The deferred restore runs when this function returns. Restore explicitly
	// before the final suite so that the final receipt proves original bytes.
	if err := restoreFile(target, original, info.Mode()); err != nil {
		result.Output += "\nrestore failed: " + err.Error()
		return result, nil
	}
	result.Restored = true
	final, err := v.VerifyCandidate(ctx, dir, baseReq)
	if err != nil {
		return nil, err
	}
	result.Final = *final
	if final.Outcome != OutcomePASS {
		result.Outcome = OutcomeBLOCKED
		result.Output += "\nrestored candidate did not PASS"
		return result, nil
	}
	result.Killed = true
	result.Outcome = OutcomePASS
	return result, nil
}

func (v *Verifier) mutationTimeout() time.Duration {
	if v != nil && v.Timeout > 0 {
		return v.Timeout
	}
	return 30 * time.Second
}

func restoreFile(path string, contents []byte, mode os.FileMode) error {
	if err := writeRegularFile(path, contents, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

func readRegularFile(path string) ([]byte, os.FileInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, nil, errors.New("mutation target must remain an Lstat regular file")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	contents, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	return contents, info, nil
}

func writeRegularFile(path string, contents []byte, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("refusing to write a non-regular mutation target")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC|syscall.O_NOFOLLOW, mode)
	if err != nil {
		return err
	}
	defer file.Close()
	written, err := file.Write(contents)
	if err != nil {
		return err
	}
	if written != len(contents) {
		return io.ErrShortWrite
	}
	return nil
}
