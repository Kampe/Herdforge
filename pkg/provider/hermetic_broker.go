package provider

import (
	"testing"
)

// WireHermeticFenceBroker starts an in-process FenceBroker against upstreamURL
// for production-shaped tests (daemon pulse, compiled claim paths). It wires
// worker client + claim-dir minter (after scrubbing env mint material).
// claimDir must be the same directory used for OpenClaimStack so leases.db is shared.
func WireHermeticFenceBroker(t testing.TB, k *KaneoProvider, upstreamURL, claimDir string) *FenceBroker {
	t.Helper()
	if k == nil {
		t.Fatal("nil KaneoProvider")
	}
	if claimDir == "" {
		t.Fatal("claimDir required")
	}
	ProvisionSharedFenceForTest(t, claimDir)
	ScrubWorkerMintEnv()
	tok := "hermetic-broker-token-min16"
	mint := "hermetic-mint-token-min16xx"
	b, err := StartFenceBroker(FenceBrokerConfig{
		ClaimDir: claimDir, ListenAddr: "127.0.0.1:0",
		Token: tok, MintToken: mint,
		UpstreamURL: upstreamURL, UpstreamProject: k.ProjectID,
	})
	if err != nil {
		t.Fatalf("StartFenceBroker: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })

	// Worker URL only — never mint token env.
	t.Setenv(envFenceBrokerURL, b.ClientBaseURL())
	t.Setenv(envFenceBrokerToken, tok)
	ScrubWorkerMintEnv()

	worker := NewFenceBrokerClientForTest(b)
	if err := ConfigureKaneoFenceBroker(k, worker); err != nil {
		t.Fatalf("ConfigureKaneoFenceBroker: %v", err)
	}
	// In-process minter only (claim-dir file is same-UID readable — not authority).
	m := newMinterForTest(b)
	if m == nil {
		t.Fatal("nil minter")
	}
	if err := AttachCoordinatorMinter(k, m); err != nil {
		t.Fatalf("AttachCoordinatorMinter: %v", err)
	}
	return b
}
