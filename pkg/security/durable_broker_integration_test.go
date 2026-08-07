package security

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func buildHerdBinary(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "herd")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/herd")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build herd: %v\n%s", err, b)
	}
	return out
}

func TestDurableBroker_CompiledIntegration_ControlShutdown(t *testing.T) {
	restoreInline := ForceInlineBrokerForTest(false)
	defer restoreInline()
	herdBin := buildHerdBinary(t)
	restoreBin := SetDurableBrokerBinaryForTest(herdBin)
	defer restoreBin()

	shared := t.TempDir()
	st, err := StartDurableBroker(shared, "tab-int-1", "ses-int-1", []string{"api.x.ai", "api.openai.com"})
	if err != nil {
		t.Fatalf("StartDurableBroker: %v", err)
	}
	if st.PID <= 0 || st.Identity == "" || st.Addr == "" || st.Token == "" {
		t.Fatalf("incomplete proxy state: %+v", st)
	}
	// Agent-visible state must not contain control token.
	raw, _ := os.ReadFile(st.StatePath)
	if strings.Contains(string(raw), "control_token") {
		t.Fatal("proxy state leaked control_token")
	}
	ctrl, err := ReadBrokerControlState(st.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := BrokerControlPing(ctrl); err != nil {
		t.Fatalf("control ping: %v", err)
	}
	// Proxy token must NOT authorize control.
	req, _ := http.NewRequest(http.MethodGet, strings.TrimRight(ctrl.ControlURL, "/")+"/ping", nil)
	req.SetBasicAuth("herd", st.Token) // agent proxy token
	resp, err := pinnedControlHTTPClient(ctrl.ControlAddr).Do(req)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode == 200 {
			t.Fatal("proxy token must not authorize control plane")
		}
	}
	if err := ProveDurableBrokerDeny(st.Addr, st.ProxyURL, "evil.example"); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if err := StopDurableBroker(st.StatePath); err != nil {
		t.Fatalf("StopDurableBroker: %v", err)
	}
	if processAlive(st.PID) {
		t.Fatalf("pid %d still alive", st.PID)
	}
}

func TestDurableBroker_MaliciousAgentCannotShutdown(t *testing.T) {
	// Inline broker with control enabled: proxy token must not control.
	b, err := StartHostAllowBroker([]string{"api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.EnableControl("id1", "tab", "ses", "", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	// Agent dials control with proxy token → 407
	c, err := net.DialTimeout("tcp", b.ControlAddr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	basic := base64.StdEncoding.EncodeToString([]byte("herd:" + b.Token))
	_, _ = io.WriteString(c, "GET /__herd_control/shutdown HTTP/1.1\r\nHost: x\r\nProxy-Authorization: Basic "+basic+"\r\nConnection: close\r\n\r\n")
	line, _ := bufio.NewReader(c).ReadString('\n')
	if strings.Contains(line, "200") {
		t.Fatalf("proxy token must not shutdown, got %q", strings.TrimSpace(line))
	}
	// Control still up
	c2, err := net.DialTimeout("tcp", b.ControlAddr(), time.Second)
	if err != nil {
		t.Fatal("control should still be listening")
	}
	_ = c2.Close()
}

func TestDurableBroker_StopRefusesUnauthenticatedPID(t *testing.T) {
	shared := t.TempDir()
	statePath := BrokerStatePath(shared, "tab-hostile")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	hostile := DurableBrokerState{
		PID:      os.Getpid(),
		Addr:     "127.0.0.1:1",
		Token:    "deadbeefdeadbeef",
		TabID:    "tab-hostile",
		Identity: "fake-identity-token",
	}
	raw, _ := json.Marshal(hostile)
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	err := StopDurableBroker(statePath)
	if err == nil {
		t.Fatal("stop must refuse without control state while pid alive")
	}
	if !processAlive(os.Getpid()) {
		t.Fatal("must not signal test process")
	}
}

func TestDurableBroker_CorruptStatePreserved(t *testing.T) {
	shared := t.TempDir()
	statePath := BrokerStatePath(shared, "tab-corrupt")
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, []byte("NOT-JSON{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := StopDurableBroker(statePath)
	if err == nil {
		t.Fatal("corrupt must fail closed")
	}
	b, _ := os.ReadFile(statePath)
	if !strings.Contains(string(b), "NOT-JSON") {
		t.Fatal("corrupt evidence not preserved")
	}
}

func TestDurableBroker_RebindTransactional(t *testing.T) {
	restoreInline := ForceInlineBrokerForTest(false)
	defer restoreInline()
	herdBin := buildHerdBinary(t)
	restoreBin := SetDurableBrokerBinaryForTest(herdBin)
	defer restoreBin()

	shared := t.TempDir()
	st, err := StartDurableBroker(shared, "prov-agent-x", "ses-rebind", []string{"api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = StopDurableBroker(BrokerStatePath(shared, "real-tab-99")) }()

	newPath := BrokerStatePath(shared, "real-tab-99")
	if err := RebindBrokerState(st.StatePath, newPath, "real-tab-99", "ses-live-1"); err != nil {
		t.Fatalf("rebind: %v", err)
	}
	back, err := ReadBrokerState(newPath)
	if err != nil {
		t.Fatal(err)
	}
	if back.TabID != "real-tab-99" {
		t.Fatal(back.TabID)
	}
	ctrl, err := ReadBrokerControlState(back.ControlPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := BrokerControlPing(ctrl); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(st.StatePath); !os.IsNotExist(err) {
		t.Fatal("old path must be gone")
	}
	if err := StopDurableBroker(newPath); err != nil {
		t.Fatal(err)
	}
}

func TestDurableBroker_RebindRejectsStatePathOnControl(t *testing.T) {
	b, err := StartHostAllowBroker([]string{"api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if err := b.EnableControl("id", "tab", "ses", "", time.Now().Unix()); err != nil {
		t.Fatal(err)
	}
	c, err := net.DialTimeout("tcp", b.ControlAddr(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	basic := base64.StdEncoding.EncodeToString([]byte("herd-ctrl:" + b.ControlToken))
	body := `{"tab_id":"x","state_path":"/tmp/evil"}`
	_, _ = io.WriteString(c, "POST /__herd_control/rebind HTTP/1.1\r\nHost: x\r\nProxy-Authorization: Basic "+basic+"\r\nContent-Type: application/json\r\nContent-Length: "+itoa(len(body))+"\r\nConnection: close\r\n\r\n"+body)
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatal("state_path on rebind must be rejected")
	}
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func TestPreferDurableBroker_NoAmbientInlineBypass(t *testing.T) {
	t.Setenv("HERD_INLINE_BROKER", "1")
	restore := ForceInlineBrokerForTest(false)
	defer restore()
	if preferDurableBroker() {
		t.Fatal("go test prefers inline")
	}
	restore2 := SetDurableBrokerBinaryForTest("/tmp/fake-herd")
	defer restore2()
	if !preferDurableBroker() {
		t.Fatal("durable binary enables durable")
	}
}

func TestStartBrokerForLaunch_DurableWithBinary(t *testing.T) {
	restoreInline := ForceInlineBrokerForTest(false)
	defer restoreInline()
	herdBin := buildHerdBinary(t)
	restoreBin := SetDurableBrokerBinaryForTest(herdBin)
	defer restoreBin()
	shared := t.TempDir()
	bl, err := StartBrokerForLaunch(shared, "tab-launch", "ses", []string{"api.x.ai"})
	if err != nil {
		t.Fatal(err)
	}
	if bl.Inline != nil || bl.StatePath == "" || bl.PID <= 0 {
		t.Fatalf("%+v", bl)
	}
	if err := bl.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPinnedNoProxyTransport_NoAmbientProxy(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go http.Serve(ln, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("pinned-ok"))
	}))
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	b, err := StartHostAllowBroker([]string{"127.0.0.1"})
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
	_, _ = io.WriteString(c, "GET http://127.0.0.1:"+port+"/x HTTP/1.1\r\nHost: 127.0.0.1:"+port+"\r\nProxy-Authorization: Basic "+basic+"\r\nConnection: close\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "pinned-ok") {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
}

func TestValidateLiveTaskLease_RejectsFabricatedGen(t *testing.T) {
	lookup := MapClaimLookup{
		"FAC-133": {TaskRef: "FAC-133", Generation: 7, ExpiresAt: time.Now().Add(time.Hour)},
	}
	if err := ValidateLiveTaskLease(t.Context(), lookup, "FAC-133", "999", false, "", ""); err == nil {
		t.Fatal("fabricated future gen must fail")
	}
	if err := ValidateLiveTaskLease(t.Context(), lookup, "FAC-133", "7", false, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLiveTaskLease(t.Context(), nil, "FAC-133", "7", false, "", ""); err == nil {
		t.Fatal("nil lookup must fail for tasks")
	}
}
