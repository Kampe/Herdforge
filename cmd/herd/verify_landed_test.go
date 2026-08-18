package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
)

// FAC-379: without a merge-admission or full binding, verify-landed must not
// invent provenance — it returns a durable reconciliation action instead.
func TestResolveVerifyLandedRequestRequiresBinding(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	_, err = resolveVerifyLandedRequest(verifyLandedBinding{}, "abc")
	if err == nil || !strings.Contains(err.Error(), "durable action") {
		t.Fatalf("expected durable action refusal without --ref, got %v", err)
	}

	_, err = resolveVerifyLandedRequest(verifyLandedBinding{Ref: "FAC-379"}, "abc")
	if err == nil || !strings.Contains(err.Error(), "durable action") {
		t.Fatalf("expected durable action when admission and flags are missing, got %v", err)
	}
}

func TestResolveVerifyLandedRequestLoadsAdmission(t *testing.T) {
	dir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(cwd) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	req := mergeadmit.Request{
		Ref: "FAC-379", TaskID: "task-1", ProviderRevision: "rev-1",
		AcceptanceDigest: mergeadmit.ComputeAcceptanceDigest("FAC-379", "task-1", "rev-1"),
		CandidateSHA:     "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:          "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Lease:            "lease-1", LeaseGeneration: 1, PatchURL: "patch-1",
		AuthorFamily: "anthropic", AuthorIdentity: "builder-1",
		Mode: mergeadmit.ModeRebase,
	}
	d := mergeadmit.Decision{
		Admitted: true, Ref: "FAC-379", CandidateSHA: req.CandidateSHA, BaseSHA: req.BaseSHA,
		Mode: mergeadmit.ModeRebase, Tier: "R2", ReviewerFam: "xai",
		VerificationDigest: "vfy-1", PolicyRevision: "policy-1",
	}
	if err := writeAdmissionRecord(".", req, d); err != nil {
		t.Fatalf("seed admission: %v", err)
	}
	if _, err := os.Stat(filepath.Join(".herd", "merge-admissions", "FAC-379.json")); err != nil {
		t.Fatalf("admission not written: %v", err)
	}

	got, err := resolveVerifyLandedRequest(verifyLandedBinding{Ref: "FAC-379"}, "cccccccccccccccccccccccccccccccccccccccc")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.CandidateSHA != req.CandidateSHA {
		t.Fatalf("candidate = %s, want admission's %s", got.CandidateSHA, req.CandidateSHA)
	}
	if got.LeaseGeneration != 1 || got.PatchURL != "patch-1" {
		t.Fatalf("binding not loaded from admission: %+v", got)
	}
}
