package security

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLeaseFromOpts_RejectsNonPositive(t *testing.T) {
	// Must NOT fabricate generation 1 for lease <=0.
	if _, err := LeaseFromOpts(0); err == nil {
		t.Fatal("lease 0 must fail closed")
	}
	if _, err := LeaseFromOpts(-1); err == nil {
		t.Fatal("lease -1 must fail closed")
	}
	s, err := LeaseFromOpts(1)
	if err != nil || s != "1" {
		t.Fatalf("lease 1: %q %v", s, err)
	}
	s, err = LeaseFromOpts(42)
	if err != nil || s != "42" {
		t.Fatalf("lease 42: %q %v", s, err)
	}
}

func TestStableStandingLease_Reusable(t *testing.T) {
	a := StableStandingLease("forge-smith")
	b := StableStandingLease("forge-smith")
	if a != b || a == "" {
		t.Fatalf("%q %q", a, b)
	}
	if strings.Contains(a, "gen-") {
		t.Fatal("standing lease must not be random gen")
	}
}

func TestDefaultHarnessAllowHosts_IncludesXAI(t *testing.T) {
	hosts := DefaultHarnessAllowHosts()
	found := false
	for _, h := range hosts {
		if h == "api.x.ai" {
			found = true
		}
	}
	if !found {
		t.Fatal("fleet xAI endpoint must be allowlisted for limited network")
	}
}

func TestProxyURL_EmbedsBasicAuth(t *testing.T) {
	b, err := StartHostAllowBroker([]string{"127.0.0.1", "api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	u := b.ProxyURL()
	if !strings.Contains(u, "herd:") || !strings.Contains(u, "@") {
		t.Fatalf("ProxyURL must embed Basic userinfo, got %q", u)
	}
	if err := ProveBrokerAllowDeny(b, "api.x.ai", "evil.example"); err != nil {
		t.Fatal(err)
	}
}

func TestReviewerNetworkIsLimitedNotOffline(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	p, err := PolicyForLane(RoleReviewer, wt, shared, "herdforge", []string{"herdforge"}, "secret", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Network != "limited" {
		t.Fatalf("reviewer must use limited for model transport, got %s", p.Network)
	}
	if len(p.NetworkAllowHosts) == 0 {
		t.Fatal("reviewer limited must have allow hosts")
	}
}

func TestResolveAndPinIP_LiteralLoopback(t *testing.T) {
	ip, err := resolveAndPinIP("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if !ip.IsLoopback() {
		t.Fatal(ip)
	}
}

func TestResolveAndPinIP_PrivateLiteralDenied(t *testing.T) {
	if _, err := resolveAndPinIP("10.0.0.1"); err == nil {
		t.Fatal("private IP must be denied")
	}
}
