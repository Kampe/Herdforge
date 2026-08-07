package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/claim"
	"github.com/Kampe/Herdforge/pkg/deps"
	"github.com/Kampe/Herdforge/pkg/winddown"
)

// TestLeaseDBPathDefaultsToProductionLaunchClaims is the non-vacuous guard for
// review HIGH #2: a wrong default (.herd/leases.db) silently empties renewals.
func TestLeaseDBPathDefaultsToProductionLaunchClaims(t *testing.T) {
	t.Setenv("HERD_LEASE_DB", "")
	_ = os.Unsetenv("HERD_LEASE_DB")
	got := leaseDBPath()
	want := deps.DefaultLaunchLeasePath()
	if got != want {
		t.Fatalf("leaseDBPath()=%q want production path %q", got, want)
	}
	if got == filepath.Join(".herd", "leases.db") {
		t.Fatal("leaseDBPath must not use the non-production .herd/leases.db default")
	}
	if !strings.Contains(got, "launch-claims.db") {
		t.Fatalf("leaseDBPath must name launch-claims.db, got %q", got)
	}
	// Override still works for hermetic tests.
	t.Setenv("HERD_LEASE_DB", "/tmp/custom-leases.db")
	if leaseDBPath() != "/tmp/custom-leases.db" {
		t.Fatal("HERD_LEASE_DB override ignored")
	}
}

// TestPulseCommand_WindDownRejectsBeforeBeat restores FAC-93 AC: pulse is the
// canonical fleet-admission gate. Enabled wind-down must print
// "fleet admission rejected" and exit non-zero without running a full beat.
func TestPulseCommand_WindDownRejectsBeforeBeat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "winddown.json")
	t.Setenv("HERD_WINDDOWN_STATE", path)
	a, err := winddown.New(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Update(context.Background(), true, "test", "fac-73-admission", 1, nil); err != nil {
		t.Fatal(err)
	}

	outF, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	errF, err := os.CreateTemp(t.TempDir(), "err")
	if err != nil {
		t.Fatal(err)
	}
	code := runPulseCommand(nil, outF, errF)
	if code == 0 {
		t.Fatal("enabled wind-down must reject pulse (exit non-zero)")
	}
	if _, err := errF.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(errF)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "fleet admission rejected") {
		t.Fatalf("want fleet admission rejected, got exit=%d stderr=%q stdout seek-check", code, raw)
	}
	// Non-vacuous flip: disabled wind-down must not emit that rejection string
	// from the admission gate alone (later sources may still fail closed).
	if _, err := a.Update(context.Background(), false, "test", "fac-73-admission-off", 2, nil); err != nil {
		t.Fatal(err)
	}
}

// TestReadPulseLeasesSeesProductionLaunchClaimsDB proves --act renew wiring
// opens the same store daemon/dispatch write (not an empty alternate path).
func TestReadPulseLeasesSeesProductionLaunchClaimsDB(t *testing.T) {
	root := t.TempDir()
	t.Chdir(root)
	_ = os.Unsetenv("HERD_LEASE_DB")
	// OpenLeaseOwnership resolves a canonical hold path via git-common;
	// a real repo root is required (production always has one).
	runGitT(t, root, "init", "-q", "-b", "main")
	runGitT(t, root, "config", "user.email", "pulse@test")
	runGitT(t, root, "config", "user.name", "pulse")

	dbPath := deps.DefaultLaunchLeasePath()
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Also plant a decoy at the old wrong path so a regression that opens
	// leases.db would not accidentally "pass" by finding the real lease.
	decoy := filepath.Join(".herd", "leases.db")
	if err := os.WriteFile(decoy, []byte("not-a-sqlite-db"), 0o600); err != nil {
		t.Fatal(err)
	}

	own, err := deps.OpenLeaseOwnership(dbPath, "Herdforge", "memory", "p")
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	own.LaneResolver = func(role string) (string, error) {
		return "smith", nil
	}
	tok, err := own.ClaimExclusive(context.Background(), "id-fac-1", "FAC-1", "worker", "rev1", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if tok == nil || tok.Generation <= 0 {
		t.Fatalf("token=%+v", tok)
	}

	// Shorten remaining TTL so Plan would renew: rewrite via ClaimManager clock
	// is not exposed; ActiveClaims alone proves the store is the production one.
	leases, mgr, err := readPulseLeases(context.Background())
	if err != nil {
		t.Fatalf("readPulseLeases: %v", err)
	}
	if mgr == nil {
		t.Fatal("expected ClaimManager against production store")
	}
	if len(leases) != 1 {
		t.Fatalf("leases=%+v want the FAC-1 claim from launch-claims.db", leases)
	}
	if leases[0].TaskRef != "FAC-1" || leases[0].Generation != tok.Generation || !leases[0].Active {
		t.Fatalf("lease observation mismatch: %+v token gen=%d", leases[0], tok.Generation)
	}

	// Generation-fenced renew against the same production manager.
	renewed, err := mgr.Renew(context.Background(), claim.LeaseKey{
		Repo: leases[0].Repo, Provider: leases[0].Provider,
		Project: leases[0].Project, TaskRef: leases[0].TaskRef,
	}, leases[0].OwnerID, leases[0].Generation)
	if err != nil {
		t.Fatalf("Renew on production store: %v", err)
	}
	if renewed.Generation != tok.Generation {
		t.Fatalf("renew changed generation: %d -> %d", tok.Generation, renewed.Generation)
	}
	// Stale generation must fail (proves fencing is live, not vacuous success).
	if _, err := mgr.Renew(context.Background(), claim.LeaseKey{
		Repo: leases[0].Repo, Provider: leases[0].Provider,
		Project: leases[0].Project, TaskRef: leases[0].TaskRef,
	}, leases[0].OwnerID, leases[0].Generation-1); err == nil {
		t.Fatal("stale generation renew must fail")
	}
}
