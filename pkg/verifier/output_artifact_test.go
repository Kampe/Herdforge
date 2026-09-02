package verifier

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const namedFailureLine = "--- FAIL: TestNamedPackageFailure (0.00s)"

func TestVerifyAndPersistFAILArtifactLookupRecoversNamedFailure(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := namedFailureRepo(t)
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewVerifierArgs([]string{"./named-fail.sh"}).VerifyAndPersist(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, store)
	if err != nil || receipt == nil || receipt.Outcome != OutcomeFAIL {
		t.Fatalf("expected persisted FAIL receipt: receipt=%+v err=%v", receipt, err)
	}
	if receipt.OutputDigest == "" {
		t.Fatal("FAIL receipt must bind an output digest")
	}

	art, err := store.LookupOutput(context.Background(), receipt.OutputDigest)
	if err != nil {
		t.Fatalf("lookup must recover FAIL process output: %v", err)
	}
	if !strings.Contains(art.Body, namedFailureLine) {
		t.Fatalf("lookup body %q does not contain named failure line", art.Body)
	}
	if art.OutputDigest != receipt.OutputDigest {
		t.Fatalf("artifact digest %q != receipt output digest %q", art.OutputDigest, receipt.OutputDigest)
	}
	if art.OriginalBytes < len(namedFailureLine) {
		t.Fatalf("original_bytes=%d, want at least named failure length", art.OriginalBytes)
	}
	info, err := os.Lstat(filepath.Join(store.outputDir(), receipt.OutputDigest))
	if err != nil {
		t.Fatalf("artifact file: %v", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("artifact path must not be a symlink")
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("artifact mode = %o, want 600", got)
	}
	dirInfo, err := os.Lstat(store.outputDir())
	if err != nil {
		t.Fatal(err)
	}
	if dirInfo.Mode()&os.ModeSymlink != 0 {
		t.Fatal("artifact directory must not be a symlink")
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("artifact directory mode = %o, want 700", got)
	}
}

func TestVerifyAndPersistDoesNotWritePASSArtifact(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := verificationRepo(t)
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := NewVerifierArgs([]string{"./check.sh"}).VerifyAndPersist(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, store)
	if err != nil || receipt.Outcome != OutcomePASS {
		t.Fatalf("expected PASS: receipt=%+v err=%v", receipt, err)
	}
	_, err = store.LookupOutput(context.Background(), receipt.OutputDigest)
	if !errors.Is(err, ErrMissingOutputArtifact) {
		t.Fatalf("PASS must not require an output artifact: %v", err)
	}
}

func TestFileReceiptStorePersistFAILRequiresOutputArtifact(t *testing.T) {
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt := fixtureFailingReceipt(strings.Repeat("a", 40), []byte(namedFailureLine+"\n"))
	receipt.outputBody = ""
	receipt.outputBytes = 0
	receipt.outputTruncated = false
	if err := store.Persist(context.Background(), receipt); err == nil {
		t.Fatal("FAIL receipt persist must not succeed without an output artifact")
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".json") {
			t.Fatalf("FAIL receipt became observable without artifact: %s", entry.Name())
		}
	}
}

func TestOutputArtifactPersistIsIdempotentAndRefusesConflict(t *testing.T) {
	store, err := NewOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(namedFailureLine + "\n")
	art := artifactFromComplete(body)
	if err := store.Persist(context.Background(), art); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(context.Background(), art); err != nil {
		t.Fatalf("same-digest rewrite must be idempotent: %v", err)
	}
	got, err := store.Lookup(context.Background(), art.OutputDigest)
	if err != nil || got.Body != art.Body || got.OriginalBytes != art.OriginalBytes || got.Truncated != art.Truncated {
		t.Fatalf("idempotent readback mismatch: got=%+v err=%v", got, err)
	}

	conflict := art
	conflict.Body = "different-body\n"
	conflict.OriginalBytes = len(conflict.Body)
	if err := store.Persist(context.Background(), conflict); err == nil {
		t.Fatal("conflicting content for an existing digest must fail closed")
	}
	got, err = store.Lookup(context.Background(), art.OutputDigest)
	if err != nil || got.Body != art.Body {
		t.Fatalf("conflict must not overwrite: got=%+v err=%v", got, err)
	}
}

func TestOutputArtifactConcurrentWritersOneWinner(t *testing.T) {
	store, err := NewOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	art := artifactFromComplete([]byte(namedFailureLine + "\n"))
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.Persist(context.Background(), art)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent same-digest writers must succeed: %v", err)
		}
	}
	got, err := store.Lookup(context.Background(), art.OutputDigest)
	if err != nil || got.Body != art.Body {
		t.Fatalf("concurrent readback: got=%+v err=%v", got, err)
	}
	entries, err := os.ReadDir(store.Dir)
	if err != nil {
		t.Fatal(err)
	}
	var files int
	for _, entry := range entries {
		if entry.Name() == art.OutputDigest {
			files++
		}
	}
	if files != 1 {
		t.Fatalf("expected one artifact file, got %d from %v", files, entries)
	}
}

func TestOutputArtifactRefuseUnsafePathsAndMismatches(t *testing.T) {
	root := t.TempDir()
	if _, err := NewOutputArtifactStore(""); err == nil {
		t.Fatal("empty store directory must fail")
	}
	if _, err := NewRepoRelativeOutputArtifactStore("/tmp/verifier-output"); err == nil {
		t.Fatal("absolute configured artifact path must fail")
	}
	store, err := NewOutputArtifactStore(filepath.Join(root, "output"))
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		art  OutputArtifact
	}{
		{name: "wrong digest", art: OutputArtifact{OutputDigest: strings.Repeat("a", 64), Body: namedFailureLine, OriginalBytes: len(namedFailureLine)}},
		{name: "uppercase digest", art: OutputArtifact{OutputDigest: strings.ToUpper(digestBytes([]byte("x"))), Body: "x", OriginalBytes: 1}},
		{name: "traversal digest", art: OutputArtifact{OutputDigest: "../" + strings.Repeat("a", 61), Body: "x", OriginalBytes: 1}},
		{name: "absolute digest", art: OutputArtifact{OutputDigest: "/tmp/" + strings.Repeat("a", 59), Body: "x", OriginalBytes: 1}},
		{name: "oversize body", art: OutputArtifact{OutputDigest: digestBytes(make([]byte, MaxRetainedOutputBytes+1)), Body: strings.Repeat("a", MaxRetainedOutputBytes+1), OriginalBytes: MaxRetainedOutputBytes + 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := store.Persist(context.Background(), tt.art); err == nil {
				t.Fatal("unsafe or mismatched artifact must fail closed")
			}
			if entries, err := os.ReadDir(store.Dir); err == nil && len(entries) != 0 {
				t.Fatalf("failed persist must not leave artifacts: %v", entries)
			}
		})
	}
}

func TestOutputArtifactRefuseSymlinkTarget(t *testing.T) {
	root := t.TempDir()
	storeDir := filepath.Join(root, "output")
	if err := os.MkdirAll(storeDir, 0o700); err != nil {
		t.Fatal(err)
	}
	store, err := NewOutputArtifactStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	art := artifactFromComplete([]byte(namedFailureLine + "\n"))
	outside := filepath.Join(root, "outside")
	if err := os.WriteFile(outside, []byte("victim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(storeDir, art.OutputDigest)); err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(context.Background(), art); err == nil {
		t.Fatal("symlink target must be refused")
	}
	got, err := os.ReadFile(outside)
	if err != nil || string(got) != "victim\n" {
		t.Fatalf("symlink victim was overwritten: %q err=%v", got, err)
	}
}

func TestOutputArtifactRefuseSymlinkStoreDir(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	store, err := NewOutputArtifactStore(linkDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Persist(context.Background(), artifactFromComplete([]byte("x"))); err == nil {
		t.Fatal("symlink store directory must be refused")
	}
}

func TestOutputArtifactPartialWriteLeavesNoDest(t *testing.T) {
	store, err := NewOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	store.createTemp = func(dir, pattern string) (*os.File, error) {
		f, err := os.CreateTemp(dir, pattern)
		if err != nil {
			return nil, err
		}
		return f, nil
	}
	store.afterWrite = func(*os.File) error {
		return errors.New("injected partial write")
	}
	art := artifactFromComplete([]byte(namedFailureLine + "\n"))
	if err := store.Persist(context.Background(), art); err == nil {
		t.Fatal("partial write must fail")
	}
	if _, err := os.Lstat(filepath.Join(store.Dir, art.OutputDigest)); !os.IsNotExist(err) {
		t.Fatalf("partial write must not install dest: %v", err)
	}
}

func TestOutputArtifactPermissionDriftFailsLookup(t *testing.T) {
	store, err := NewOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	art := artifactFromComplete([]byte(namedFailureLine + "\n"))
	if err := store.Persist(context.Background(), art); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, art.OutputDigest)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Lookup(context.Background(), art.OutputDigest); err == nil {
		t.Fatal("permission drift must fail closed")
	}
}

func TestOutputArtifactTruncationMetadata(t *testing.T) {
	store, err := NewOutputArtifactStore(filepath.Join(t.TempDir(), "output"))
	if err != nil {
		t.Fatal(err)
	}
	complete := append([]byte(namedFailureLine+"\n"), make([]byte, MaxRetainedOutputBytes)...)
	art := artifactFromComplete(complete)
	if !art.Truncated || len(art.Body) > MaxRetainedOutputBytes || art.OriginalBytes != len(complete) {
		t.Fatalf("truncated artifact metadata: %+v len=%d", art, len(art.Body))
	}
	if err := store.Persist(context.Background(), art); err != nil {
		t.Fatal(err)
	}
	got, err := store.Lookup(context.Background(), art.OutputDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Truncated || got.OriginalBytes != len(complete) || !strings.Contains(got.Body, namedFailureLine) {
		t.Fatalf("lookup truncation metadata: %+v", got)
	}
	if !strings.Contains(got.Body, "[output truncated]") {
		t.Fatalf("truncated body must mark truncation explicitly: %q", got.Body)
	}
}

func TestLookupMissingOutputArtifactIsEvidenceGap(t *testing.T) {
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt := fixtureFailingReceipt(strings.Repeat("b", 40), []byte(namedFailureLine+"\n"))
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(store.Dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, strings.TrimPrefix(receipt.Digest, "sha256:")+".json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), receipt.Digest)
	if err != nil || loaded.Digest != receipt.Digest || loaded.Outcome != OutcomeFAIL {
		t.Fatalf("legacy FAIL receipt must still parse: loaded=%+v err=%v", loaded, err)
	}
	_, err = store.LookupOutput(context.Background(), loaded.OutputDigest)
	if !errors.Is(err, ErrMissingOutputArtifact) {
		t.Fatalf("missing sidecar must be an explicit evidence gap: %v", err)
	}
	if msg := FormatOutputEvidence(OutputArtifact{}, err); !strings.Contains(msg, "evidence gap") {
		t.Fatalf("coordinator diagnostic must name the evidence gap: %q", msg)
	}
}

func TestAdmissionUnchangedForFAILWithArtifact(t *testing.T) {
	parallelVerifierStress(t)
	dir, candidate := namedFailureRepo(t)
	store, err := NewFileReceiptStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fail, err := NewVerifierArgs([]string{"./named-fail.sh"}).VerifyAndPersist(context.Background(), dir, VerificationRequest{
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
		LeaseGeneration:   "lease-1",
	}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewReceiptAdmission(store).RequireCurrentPassing(context.Background(), dir, fail.Digest); err == nil || !strings.Contains(err.Error(), "not PASS") {
		t.Fatalf("FAIL with artifact must still be refused by admission: %v", err)
	}
}

func namedFailureRepo(t *testing.T) (string, string) {
	t.Helper()
	dir, _ := verificationRepo(t)
	writeExecutable(t, filepath.Join(dir, "named-fail.sh"), "#!/bin/sh\necho '"+namedFailureLine+"'\nexit 1\n")
	git(t, dir, "add", "named-fail.sh")
	git(t, dir, "commit", "-q", "-m", "named failure")
	return dir, gitOutput(t, dir, "rev-parse", "HEAD")
}

func fixtureFailingReceipt(candidate string, output []byte) Receipt {
	result := &Result{
		Passed:       false,
		Outcome:      OutcomeFAIL,
		Output:       string(output),
		OutputDigest: digestBytes(output),
		ExitCode:     1,
	}
	bindHashedOutput(result, output)
	return makeReceipt(VerificationRequest{
		TaskRef:           "FAC-712",
		LeaseGeneration:   "lease-1",
		CandidateSHA:      candidate,
		EnvironmentPolicy: EnvironmentPolicyInherited,
	}, []string{"./named-fail.sh"}, result, OutcomeFAIL)
}

func artifactFromComplete(complete []byte) OutputArtifact {
	body := boundedOutput(complete)
	return OutputArtifact{
		Version:       1,
		OutputDigest:  digestBytes(complete),
		Body:          body,
		Truncated:     len(complete) > MaxRetainedOutputBytes,
		OriginalBytes: len(complete),
	}
}
