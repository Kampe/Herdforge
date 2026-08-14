package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/remoteci"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

func TestMergeAdmitPolicyRequiredRemoteCIMissingSettlementBlocks(t *testing.T) {
	gate := &mergeadmit.Gate{Policy: preflight.MergePolicy{RemoteCI: preflight.RemoteCIPolicy{Required: true, RequiredChecks: []string{"build"}}}}
	req := mergeadmit.Request{CandidateSHA: strings.Repeat("a", 40)}
	ledgerPath := filepath.Join(t.TempDir(), ".herd", "remote-ci.jsonl")
	err := bindRemoteCIAdmission(gate, &req, ledgerPath, 1)
	if err == nil || req.RemoteCI != nil || gate.RemoteCIPolicyRevision == "" {
		t.Fatalf("missing remote settlement admitted: request=%+v gate=%+v err=%v", req.RemoteCI, gate, err)
	}

	repo, err := toolchild.RepositoryIdentity(".")
	if err != nil {
		t.Fatal(err)
	}
	binding := remoteci.Binding{Repository: repo, CandidateSHA: req.CandidateSHA, PolicyRevision: remoteci.Revision("merge-policy-v1", "build"), Attempt: 1, RequiredChecks: []string{"build"}}
	store, err := remoteci.Open(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Register(binding); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PersistTerminal(remoteci.Settlement{Version: remoteci.Version1, Binding: binding, State: remoteci.StatePassed}); err != nil {
		t.Fatal(err)
	}
	if err := bindRemoteCIAdmission(gate, &req, ledgerPath, 1); err != nil || req.RemoteCI == nil || gate.RemoteCIRepository != repo {
		t.Fatalf("matching passed settlement rejected: req=%+v gate=%+v err=%v", req.RemoteCI, gate, err)
	}

	stale := mergeadmit.Request{CandidateSHA: strings.Repeat("b", 40)}
	if err := bindRemoteCIAdmission(gate, &stale, ledgerPath, 1); err == nil {
		t.Fatal("stale candidate settled from another SHA")
	}
	wrongChecks := &mergeadmit.Gate{Policy: preflight.MergePolicy{RemoteCI: preflight.RemoteCIPolicy{Required: true, RequiredChecks: []string{"lint"}}}}
	if err := bindRemoteCIAdmission(wrongChecks, &req, ledgerPath, 1); err == nil {
		t.Fatal("settlement with another required-check set was accepted")
	}
}
