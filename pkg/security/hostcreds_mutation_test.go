package security

import (
	"errors"
	"net"
	"os"
	"strings"
	"testing"
)

func TestMutation_NoAPIKeyPlant(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer mut")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m1", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		t.Fatal("MUTATION: API keys or proxy bearer planted:", err)
	}
}

func TestMutation_PerKindIsolation(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	_ = v.InstallTestSecret("api.openai.com", "Bearer y")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m2", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if err := ProveMITMExactHost(sess.Mitm, "api.openai.com"); err != nil {
		t.Fatal("MUTATION: cross-provider CONNECT allowed:", err)
	}
}

func TestMutation_LiveNeedsBoundary(t *testing.T) {
	_ = os.Unsetenv(EnvBrokerUID)
	_ = os.Unsetenv(EnvAllowSameUIDTest)
	_, _, _, err := StartAuthorLive(LiveConfig{Kind: "grok", SessionID: "m3", Prompt: "hi"})
	if err == nil {
		t.Fatal("MUTATION: live without boundary")
	}
	if !errors.Is(err, ErrHostCredsBlocked) {
		if _, ok := err.(*BlockedError); !ok {
			t.Fatal(err)
		}
	}
}

func TestMutation_LiveNeedsRealKind(t *testing.T) {
	for _, k := range []string{"fake", "test", "opencode"} {
		_, _, _, err := StartAuthorLive(LiveConfig{Kind: k, Prompt: "x"})
		if err == nil {
			t.Fatal("MUTATION: kind allowed", k)
		}
	}
}

func TestMutation_PlaintextHTTPNoInject(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m4", Authority: v, EnableOracle: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 32)
		_, _ = c.Read(buf)
		_, _ = c.Write([]byte("HTTP/1.0 200 OK\r\n\r\nok"))
	}()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	sess.Oracle.allowLoopback = true
	sess.Oracle.Hosts = append(sess.Oracle.Hosts, "127.0.0.1")
	sess.Oracle.Rules = RequestRulesForKind("fake")
	_ = v.InstallTestSecret("127.0.0.1", "Bearer x")
	sess.Oracle.dialHook = func(network, addr string) (net.Conn, error) {
		return net.Dial("tcp", net.JoinHostPort("127.0.0.1", port))
	}
	sess.Oracle.resolveHook = func(host string) (net.IP, error) {
		return net.ParseIP("127.0.0.1"), nil
	}
	r, err := CallOracle(sess.Oracle.SocketPath(), OracleRequest{
		SessionID: sess.ID, Host: "127.0.0.1", Method: "POST", Path: "/v1/chat/completions", Body: `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if r.OK {
		t.Fatal("MUTATION: plaintext credentialed path OK")
	}
}

func TestMutation_SessionIDOmit(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m5", Authority: v, EnableOracle: true})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	r, _ := CallOracle(sess.Oracle.SocketPath(), OracleRequest{Host: "api.x.ai", Method: "POST", Path: "/v1/chat/completions"})
	if r.OK {
		t.Fatal("MUTATION: omitted session_id accepted")
	}
}

func TestMutation_PromptConsumedRequiresPID(t *testing.T) {
	v := NewTestCredentialVault()
	_ = v.InstallTestSecret("api.x.ai", "Bearer x")
	sess, err := StartHostCredsSession(SessionConfig{Kind: "grok", SessionID: "m6", Authority: v})
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	_ = sess.ConsumePrompt("not enough")
	if sess.PromptConsumed() {
		t.Fatal("MUTATION: ConsumePrompt alone counts as consumed")
	}
	if err := sess.RecordHarnessPrompt("real", 0); err == nil {
		t.Fatal("MUTATION: pid 0 accepted")
	}
}

func TestMutation_Redaction(t *testing.T) {
	out := RedactSecrets(`Bearer sk-abc123XYZ and {"api_key":"sk-othersecret99"}`)
	if strings.Contains(out, "sk-abc") || strings.Contains(out, "sk-other") {
		t.Fatal(out)
	}
}
