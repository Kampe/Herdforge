package verifier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

type Result struct {
	Passed   bool
	Outcome  Outcome
	Output   string
	ExitCode int
	Duration time.Duration
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
	EnvironmentPolicy string
	Artifacts         []string
}

// Receipt is durable evidence for one command run against one candidate.
// Digest is SHA-256 over the canonical JSON form of the receipt with Digest
// omitted. Output is represented by OutputDigest so receipts stay bounded.
type Receipt struct {
	Version           int           `json:"version"`
	TaskRef           string        `json:"task_ref"`
	LeaseGeneration   string        `json:"lease_generation"`
	CandidateSHA      string        `json:"candidate_sha"`
	BaseSHA           string        `json:"base_sha,omitempty"`
	Command           []string      `json:"argv"`
	ExitCode          int           `json:"exit_code"`
	Duration          time.Duration `json:"duration_ns"`
	EnvironmentPolicy string        `json:"environment_policy"`
	Artifacts         []string      `json:"artifacts,omitempty"`
	OutputDigest      string        `json:"output_digest,omitempty"`
	Outcome           Outcome       `json:"outcome"`
	Digest            string        `json:"digest"`
}

type receiptForDigest struct {
	Version           int           `json:"version"`
	TaskRef           string        `json:"task_ref"`
	LeaseGeneration   string        `json:"lease_generation"`
	CandidateSHA      string        `json:"candidate_sha"`
	BaseSHA           string        `json:"base_sha,omitempty"`
	Command           []string      `json:"argv"`
	ExitCode          int           `json:"exit_code"`
	Duration          time.Duration `json:"duration_ns"`
	EnvironmentPolicy string        `json:"environment_policy"`
	Artifacts         []string      `json:"artifacts,omitempty"`
	OutputDigest      string        `json:"output_digest,omitempty"`
	Outcome           Outcome       `json:"outcome"`
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
	if v == nil {
		return nil, errors.New("nil verifier")
	}
	if v.commandErr != nil {
		return nil, v.commandErr
	}
	if len(v.Argv) == 0 {
		return nil, errors.New("verification command is empty")
	}

	started := time.Now()
	cmd := exec.CommandContext(ctx, v.Argv[0], v.Argv[1:]...)
	cmd.Dir = dir
	// A canceled shell can leave grandchildren holding CombinedOutput's pipe
	// open (for example, `sh -c 'sleep 3'`). WaitDelay bounds that wait and
	// keeps the mutation transaction's restoration defer reachable.
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.CombinedOutput()
	result := &Result{
		Passed:   err == nil,
		Outcome:  OutcomePASS,
		Output:   string(output),
		ExitCode: exitCode(cmd, err),
		Duration: time.Since(started),
	}
	if err != nil {
		result.Outcome = OutcomeFAIL
		if ctx.Err() != nil || cmd.ProcessState == nil {
			result.Outcome = OutcomeBLOCKED
		}
		result.Output = fmt.Sprintf("verification failed: %v\noutput:\n%s", err, string(output))
	}
	return result, nil
}

// VerifyCandidate runs the configured argv only when dir is a clean checkout
// pinned to req.CandidateSHA. A command failure is FAIL. A dirty checkout,
// SHA mismatch, malformed execution, timeout, or cancellation is BLOCKED.
func (v *Verifier) VerifyCandidate(ctx context.Context, dir string, req VerificationRequest) (*Receipt, error) {
	if err := validateRequest(req); err != nil {
		return nil, err
	}
	if err := requireCleanCandidate(dir, req.CandidateSHA); err != nil {
		return blockedReceipt(req, v.argv(), 0, nil, err), nil
	}

	result, err := v.Execute(ctx, dir)
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
	if r.Digest == "" || r.Digest != digestReceipt(r) {
		return errors.New("receipt digest is invalid")
	}
	if err := requireCleanCandidate(dir, r.CandidateSHA); err != nil {
		return fmt.Errorf("receipt candidate is no longer current: %w", err)
	}
	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

func (r Receipt) MarshalJSON() ([]byte, error) {
	type alias Receipt
	return json.Marshal(alias(r))
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
	return append([]string(nil), v.Argv...)
}

func validateRequest(req VerificationRequest) error {
	if !validSHA(req.CandidateSHA) {
		return errors.New("candidate SHA must be the exact 40-character commit SHA")
	}
	if req.BaseSHA != "" && !validSHA(req.BaseSHA) {
		return errors.New("base SHA must be the exact 40-character commit SHA")
	}
	return nil
}

func validSHA(sha string) bool {
	if len(sha) != 40 {
		return false
	}
	_, err := hex.DecodeString(sha)
	return err == nil
}

func currentSHA(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "--verify", "HEAD^{commit}")
	cmd.Dir = dir
	out, err := cmd.Output()
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
	cmd := exec.Command("git", "status", "--porcelain", "--untracked-files=all")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("read candidate status: %w", err)
	}
	if strings.TrimSpace(string(out)) != "" {
		return fmt.Errorf("candidate worktree is dirty: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

func makeReceipt(req VerificationRequest, argv []string, result *Result, outcome Outcome) Receipt {
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
		OutputDigest:      digestBytes([]byte(result.Output)),
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
	return filepath.Join(root, clean), nil
}

// MutationRequest describes one bounded, reversible mutant application.
type MutationRequest struct {
	TaskRef           string
	LeaseGeneration   string
	CandidateSHA      string
	BaseSHA           string
	EnvironmentPolicy string
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
func (v *Verifier) RunMutationCheckForCandidate(ctx context.Context, dir string, req MutationRequest) (*MutationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
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

	original, err := os.ReadFile(target)
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
	info, err := os.Stat(target)
	if err != nil {
		return nil, fmt.Errorf("stat mutation target: %w", err)
	}

	defer func() {
		if restoreErr := restoreFile(target, original, info.Mode()); restoreErr == nil {
			result.Restored = true
		} else {
			result.Output += "\nrestore failed: " + restoreErr.Error()
		}
	}()
	if err := os.WriteFile(target, []byte(req.MutantCode), info.Mode()); err != nil {
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
	if err := os.WriteFile(path, contents, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
