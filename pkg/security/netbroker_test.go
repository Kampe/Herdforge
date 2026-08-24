package security

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

func TestHostAllowBroker_PositiveNegative(t *testing.T) {
	b, err := StartHostAllowBroker([]string{"api.openai.com", "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := ProveBrokerAllowDeny(b, "api.openai.com", "evil.example"); err != nil {
		t.Fatal(err)
	}
	if b.HostAllowed("evil.example") {
		t.Fatal("evil must be denied")
	}
	if !b.HostAllowed("api.openai.com") {
		t.Fatal("api.openai.com must be allowed")
	}
	if !b.HostAllowed("127.0.0.1") {
		t.Fatal("loopback always allowed")
	}
}

func TestForward_RejectsNonHTTPSchemes(t *testing.T) {
	b, err := StartHostAllowBroker([]string{"example.com"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	c, err := net.DialTimeout("tcp", b.Addr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + b.Token))
	_, _ = fmt.Fprintf(c, "GET ftp://example.com/x HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n", basic)
	line, err := bufio.NewReader(c).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(line, "400") {
		t.Fatalf("want 400 for ftp scheme, got %q", strings.TrimSpace(line))
	}
}

func TestControlPing_RequiresAuth(t *testing.T) {
	b, err := StartHostAllowBroker([]string{"api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.EnableControl("ident-1", "tab-1", "ses", "", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// control on separate listener
	// No auth
	c, err := net.DialTimeout("tcp", b.ControlAddr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprintf(c, "GET /__herd_control/ping HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	_ = c.Close()
	if !strings.Contains(line, "407") {
		t.Fatalf("want 407, got %q", strings.TrimSpace(line))
	}
}
