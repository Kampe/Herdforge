package security

import (
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLocalhostCanary_DeniedByBroker(t *testing.T) {
	// Broker allowlist is model hosts only — arbitrary localhost must 403.
	b, err := StartHostAllowBroker([]string{"api.x.ai", "api.openai.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if b.HostAllowed("127.0.0.1") || b.HostAllowed("localhost") {
		t.Fatal("generic localhost must not be an allowed CONNECT destination")
	}
	if err := ProveDurableBrokerDeny(b.Addr(), b.ProxyURL(), "evil.example"); err != nil {
		t.Fatal(err)
	}
}

func TestBrokerForLaunch_InlineUnderTests(t *testing.T) {
	// go test binary uses inline broker (not durable process).
	shared := t.TempDir()
	bl, err := StartBrokerForLaunch(shared, "tab-test", "ses-test", []string{"api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	defer bl.Close()
	if bl.Endpoint == "" || bl.ProxyURL == "" {
		t.Fatal("empty endpoint/proxy")
	}
	// Can dial the broker listen port.
	c, err := net.DialTimeout("tcp", bl.Endpoint, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
}

func TestUnknownRole_FailClosed(t *testing.T) {
	shared := t.TempDir()
	wt := filepath.Join(shared, "wt")
	_ = os.MkdirAll(wt, 0o755)
	if _, err := PolicyForLane("", wt, shared, "herdforge", []string{"herdforge"}, "secret", nil); err == nil {
		t.Fatal("empty role must block")
	}
	if _, err := PolicyForLane("not-a-role", wt, shared, "herdforge", []string{"herdforge"}, "secret", nil); err == nil {
		t.Fatal("unknown role must block")
	}
}

func TestDefaultHarnessAllowHosts_NoGenericLocalhost(t *testing.T) {
	for _, h := range DefaultHarnessAllowHosts() {
		if h == "127.0.0.1" || h == "localhost" || h == "::1" {
			t.Fatalf("allow hosts must not include generic loopback destination %q", h)
		}
	}
}
