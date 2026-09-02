package verifier

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
)

const outputArtifactRelDir = "output"

var (
	// ErrMissingOutputArtifact is the explicit evidence gap for a receipt
	// whose output_digest has no sidecar. Legacy FAIL/BLOCKED receipts parse
	// without one; lookup must not invent output.
	ErrMissingOutputArtifact = errors.New("missing output artifact for output_digest (evidence gap)")
	// ErrInvalidOutputDigest means the key is not a lowercase 64-hex digest.
	ErrInvalidOutputDigest = errors.New("output digest must be a lowercase 64-hex sha256")
)

// OutputArtifact is the bounded, content-addressed diagnostic sidecar for a
// verifier process output. Receipt JSON stays digest-only; this file is the
// evidence used to name failing packages and tests.
type OutputArtifact struct {
	Version       int    `json:"version"`
	OutputDigest  string `json:"output_digest"`
	Body          string `json:"body"`
	Truncated     bool   `json:"truncated"`
	OriginalBytes int    `json:"original_bytes"`
}

// OutputArtifactStore persists artifacts as exclusively-created mode-0600
// files named by the lowercase 64-hex output digest.
type OutputArtifactStore struct {
	Dir        string
	createTemp func(string, string) (*os.File, error)
	link       func(string, string) error
	afterWrite func(*os.File) error
}

func NewOutputArtifactStore(dir string) (*OutputArtifactStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, errors.New("output artifact store directory is empty")
	}
	return &OutputArtifactStore{Dir: filepath.Clean(dir)}, nil
}

// NewRepoRelativeOutputArtifactStore refuses absolute configured paths so the
// store layout stays repository-relative.
func NewRepoRelativeOutputArtifactStore(rel string) (*OutputArtifactStore, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return nil, errors.New("output artifact path is empty")
	}
	if filepath.IsAbs(rel) {
		return nil, errors.New("output artifact path must be repository-relative")
	}
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("output artifact path escapes repository")
	}
	return NewOutputArtifactStore(clean)
}

func (s *OutputArtifactStore) Persist(ctx context.Context, art OutputArtifact) error {
	if s == nil {
		return errors.New("nil output artifact store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateOutputArtifact(art); err != nil {
		return err
	}
	path, err := s.pathFor(art.OutputDigest)
	if err != nil {
		return err
	}
	data, err := json.Marshal(art)
	if err != nil {
		return fmt.Errorf("encode output artifact: %w", err)
	}
	if err := s.prepareDir(); err != nil {
		return err
	}
	if err := s.refuseExistingUnsafe(path); err != nil {
		return err
	}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, data) {
			return s.readback(ctx, art.OutputDigest, data)
		}
		return errors.New("output artifact digest already exists with different content")
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("inspect output artifact: %w", err)
	}

	createTemp := s.createTemp
	if createTemp == nil {
		createTemp = os.CreateTemp
	}
	link := s.link
	if link == nil {
		link = os.Link
	}
	tmp, err := createTemp(s.Dir, ".output-*.tmp")
	if err != nil {
		return fmt.Errorf("create output artifact temporary file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect output artifact temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write output artifact: %w", err)
	}
	if s.afterWrite != nil {
		if err := s.afterWrite(tmp); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("write output artifact: %w", err)
		}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync output artifact: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close output artifact: %w", err)
	}
	if err := link(tmpName, path); err != nil {
		if !os.IsExist(err) {
			return fmt.Errorf("install output artifact: %w", err)
		}
		if err := s.refuseExistingUnsafe(path); err != nil {
			return err
		}
		existing, readErr := os.ReadFile(path)
		if readErr != nil {
			return fmt.Errorf("read competing output artifact: %w", readErr)
		}
		if !bytes.Equal(existing, data) {
			return errors.New("output artifact digest already exists with different content")
		}
		return s.readback(ctx, art.OutputDigest, data)
	}
	return s.readback(ctx, art.OutputDigest, data)
}

func (s *OutputArtifactStore) Lookup(ctx context.Context, digest string) (OutputArtifact, error) {
	var art OutputArtifact
	if s == nil {
		return art, errors.New("nil output artifact store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return art, err
	}
	path, err := s.pathFor(digest)
	if err != nil {
		return art, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return art, ErrMissingOutputArtifact
		}
		return art, fmt.Errorf("stat output artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return art, errors.New("output artifact path is a symlink")
	}
	if !info.Mode().IsRegular() {
		return art, errors.New("output artifact is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return art, fmt.Errorf("output artifact permission drift: mode %o", info.Mode().Perm())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return art, fmt.Errorf("read output artifact: %w", err)
	}
	if err := json.Unmarshal(data, &art); err != nil {
		return art, fmt.Errorf("decode output artifact: %w", err)
	}
	canonical, err := canonicalOutputDigest(digest)
	if err != nil {
		return art, err
	}
	if art.OutputDigest != canonical {
		return art, errors.New("output artifact digest does not match filename")
	}
	if err := validateOutputArtifact(art); err != nil {
		return art, err
	}
	return art, nil
}

func (s *OutputArtifactStore) readback(ctx context.Context, digest string, want []byte) error {
	got, err := s.Lookup(ctx, digest)
	if err != nil {
		return fmt.Errorf("output artifact readback: %w", err)
	}
	data, err := json.Marshal(got)
	if err != nil {
		return fmt.Errorf("encode output artifact readback: %w", err)
	}
	if !bytes.Equal(data, want) {
		return errors.New("output artifact readback does not match written content")
	}
	return nil
}

func (s *OutputArtifactStore) prepareDir() error {
	info, err := os.Lstat(s.Dir)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("output artifact store directory is a symlink")
		}
		if !info.IsDir() {
			return errors.New("output artifact store path is not a directory")
		}
	case os.IsNotExist(err):
		if err := os.MkdirAll(s.Dir, 0o700); err != nil {
			return fmt.Errorf("create output artifact store: %w", err)
		}
		info, err = os.Lstat(s.Dir)
		if err != nil {
			return fmt.Errorf("stat output artifact store: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("output artifact store directory is a symlink")
		}
	default:
		return fmt.Errorf("stat output artifact store: %w", err)
	}
	if err := os.Chmod(s.Dir, 0o700); err != nil {
		return fmt.Errorf("protect output artifact store: %w", err)
	}
	return nil
}

func (s *OutputArtifactStore) refuseExistingUnsafe(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("stat output artifact: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return errors.New("output artifact path is a symlink")
	}
	return nil
}

func (s *OutputArtifactStore) pathFor(digest string) (string, error) {
	canonical, err := canonicalOutputDigest(digest)
	if err != nil {
		return "", err
	}
	if filepath.IsAbs(canonical) {
		return "", errors.New("output digest must not be an absolute path")
	}
	dest := filepath.Join(s.Dir, canonical)
	if !pathWithin(s.Dir, dest) {
		return "", errors.New("output artifact path escapes store")
	}
	return dest, nil
}

func canonicalOutputDigest(digest string) (string, error) {
	digest = strings.TrimSpace(digest)
	if digest == "" || filepath.IsAbs(digest) || strings.ContainsRune(digest, filepath.Separator) {
		return "", ErrInvalidOutputDigest
	}
	if len(digest) != 64 {
		return "", ErrInvalidOutputDigest
	}
	for _, r := range digest {
		if !unicode.Is(unicode.ASCII_Hex_Digit, r) || unicode.IsUpper(r) {
			return "", ErrInvalidOutputDigest
		}
	}
	return digest, nil
}

func validateOutputArtifact(art OutputArtifact) error {
	if art.Version != 1 {
		return fmt.Errorf("unsupported output artifact version %d", art.Version)
	}
	digest, err := canonicalOutputDigest(art.OutputDigest)
	if err != nil {
		return err
	}
	if len(art.Body) > MaxRetainedOutputBytes {
		return errors.New("output artifact exceeds retained output bound")
	}
	if art.Truncated {
		if art.OriginalBytes <= MaxRetainedOutputBytes {
			return errors.New("truncated output artifact must record original size above the retained bound")
		}
		if !strings.Contains(art.Body, "[output truncated]") {
			return errors.New("truncated output artifact must mark truncation explicitly")
		}
		return nil
	}
	if art.OriginalBytes != len(art.Body) {
		return errors.New("untruncated output artifact original size must equal body length")
	}
	if digestBytes([]byte(art.Body)) != digest {
		return errors.New("output digest does not match artifact body")
	}
	return nil
}

func (s *FileReceiptStore) outputDir() string {
	if s == nil {
		return ""
	}
	return filepath.Join(s.Dir, outputArtifactRelDir)
}

func (s *FileReceiptStore) outputStore() *OutputArtifactStore {
	if s == nil {
		return nil
	}
	return &OutputArtifactStore{Dir: s.outputDir()}
}

func (s *FileReceiptStore) LookupOutput(ctx context.Context, digest string) (OutputArtifact, error) {
	if s == nil {
		return OutputArtifact{}, errors.New("nil receipt store")
	}
	return s.outputStore().Lookup(ctx, digest)
}

func (s *FileReceiptStore) persistRequiredOutput(ctx context.Context, receipt Receipt) error {
	if receipt.OutputDigest == "" {
		return errors.New("FAIL/BLOCKED receipt is missing output digest")
	}
	art := OutputArtifact{
		Version:       1,
		OutputDigest:  receipt.OutputDigest,
		Body:          receipt.outputBody,
		Truncated:     receipt.outputTruncated,
		OriginalBytes: receipt.outputBytes,
	}
	if err := s.outputStore().Persist(ctx, art); err != nil {
		return err
	}
	if _, err := s.LookupOutput(ctx, receipt.OutputDigest); err != nil {
		return fmt.Errorf("output artifact readback: %w", err)
	}
	return nil
}

func artifactFromResult(result *Result) (body string, original int, truncated bool) {
	if result == nil {
		return "", 0, false
	}
	if result.outputBound {
		return result.outputBody, result.outputBytes, result.outputTruncated
	}
	complete := []byte(result.Output)
	return boundedOutput(complete), len(complete), len(complete) > MaxRetainedOutputBytes
}

func bindHashedOutput(result *Result, complete []byte) {
	if result == nil {
		return
	}
	result.OutputDigest = digestBytes(complete)
	result.outputBody = boundedOutput(complete)
	result.outputBytes = len(complete)
	result.outputTruncated = len(complete) > MaxRetainedOutputBytes
	result.outputBound = true
}

func newOutputResult(outcome Outcome, complete []byte, exitCode int, duration time.Duration) *Result {
	result := &Result{
		Passed:   outcome == OutcomePASS,
		Outcome:  outcome,
		Output:   boundedOutput(complete),
		ExitCode: exitCode,
		Duration: duration,
	}
	bindHashedOutput(result, complete)
	return result
}

// FormatOutputEvidence is the coordinator-facing diagnostic for a lookup.
// Missing sidecars stay an explicit evidence gap; the body is the bounded
// original process output.
func FormatOutputEvidence(art OutputArtifact, err error) string {
	if err != nil {
		if errors.Is(err, ErrMissingOutputArtifact) {
			return ErrMissingOutputArtifact.Error()
		}
		return "output artifact unavailable"
	}
	return art.Body
}
