package signerboundary

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// FAC-169 adversarial: a hostile same-UID worker process attempts
// (1) coordinator attach/read and (2) exact sign-verdict induction.
//
// Kernel topology: S = test process (serve), R synthetic, B = worker uid.
// Without setuid we cannot become R; legitimate success uses peer injection
// only for the authorized path. Hostile path uses REAL SO_PEERCRED from a
// spawned worker whose uid equals B when B==Getuid(), or equals S (denied).
//
// Same-UID attach to a coordinator that holds session material is expected to
// SUCCEED on many hosts — that is why R must differ from B (topology). The
// test asserts that failure mode and that sign-verdict induction is denied.

func TestHostileWorkerProcess_SignVerdictInductionDenied(t *testing.T) {
	me := os.Getuid()
	// B is a synthetic builder; real worker peer will be me (S). Server still
	// denies non-R peers. Additionally inject builder for explicit B path.
	topo := Topology{
		SignerUID:    me,
		RequesterUID: me + 300001,
		BuilderUID:   me + 300002,
		SocketGID:    os.Getgid(),
	}
	keyPath, sock, sk := testKeyAndSocket(t)
	// Real peer path: no override — hostile child gets real SO_PEERCRED.
	srv, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() { _ = srv.Run() }()
	time.Sleep(40 * time.Millisecond)

	// Legitimate reviewer path (injected R only — kernel cannot spoof UID here).
	reqUID := topo.RequesterUID
	_ = srv.Close()
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")
	srvOK, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		TestPeerUIDOverride: &reqUID, NonceLedgerPath: sock + ".nonces",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srvOK.Close()
	go func() { _ = srvOK.Run() }()
	time.Sleep(40 * time.Millisecond)

	vOK := sampleVerdict("legit")
	vOK.Nonce = "hostile-test-legit-001"
	sig, err := signRequestOverIPC(sock, sk, &vOK)
	if err != nil || len(sig) == 0 {
		t.Fatalf("legitimate requester must sign: %v", err)
	}

	// Restart with REAL peer creds for hostile worker subprocess.
	_ = srvOK.Close()
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")
	srvHost, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		NonceLedgerPath: sock + ".nonces",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srvHost.Close()
	go func() { _ = srvHost.Run() }()
	time.Sleep(40 * time.Millisecond)

	// Hostile worker: same executable family, no HERD_ROLE authority, tries
	// exact sign-verdict with known session MAC bytes (worst case theft).
	helper := filepath.Join(t.TempDir(), "hostile_worker_helper.go")
	if err := os.WriteFile(helper, []byte(hostileWorkerHelperSrc), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "hostile-worker")
	build := exec.Command("go", "build", "-o", bin, helper)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hostile worker: %v\n%s", err, out)
	}

	hReq := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"patch-1", "APPROVED", "session-ok", nil,
	)
	hReq.Nonce = "hostile-worker-nonce-001"
	mac := sk.BindRequestMAC(hReq)

	cmd := exec.Command(bin, sock, mac, hReq.Nonce)
	out, err := cmd.CombinedOutput()
	// Helper exits 2 on structured UNAUTHORIZED_PEER (expected denial).
	if err == nil {
		t.Fatalf("hostile worker sign-verdict must fail; out=%s", out)
	}
	if !strings.Contains(string(out), ErrCodeUnauthorizedPeer) &&
		!strings.Contains(string(out), "UNAUTHORIZED_PEER") {
		t.Fatalf("hostile worker must report UNAUTHORIZED_PEER, got err=%v out=%s", err, out)
	}

	// Explicit builder-UID induction (injected): still denied with good MAC.
	builder := topo.BuilderUID
	_ = srvHost.Close()
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")
	srvB, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		TestPeerUIDOverride: &builder,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srvB.Close()
	go func() { _ = srvB.Run() }()
	time.Sleep(40 * time.Millisecond)

	vB := sampleVerdict("builder-induct")
	vB.Nonce = "builder-induct-001"
	code, err := dialForErrorCode(sock, vB, sk.BindRequestMAC(vB))
	if err == nil || code != ErrCodeUnauthorizedPeer {
		t.Fatalf("builder induction: want UNAUTHORIZED_PEER code=%q err=%v", code, err)
	}
}

func TestSameUIDAttach_ProvesTopologyMustSeparateRequester(t *testing.T) {
	// Coordinator-like process holds a marker in memory; hostile sibling same-UID
	// attempts PT_ATTACH. Success means FD/session-in-memory is not an OS boundary
	// under same-UID — RequireTopology must keep R != B.
	me := os.Getuid()
	t.Setenv(EnvSignerUID, itoa(me+1))
	if me+1 == 0 {
		t.Setenv(EnvSignerUID, "2")
	}
	t.Setenv(EnvRequesterUID, itoa(me))
	t.Setenv(EnvBuilderUID, itoa(me)) // hostile co-location
	if _, err := RequireTopology(); err == nil {
		t.Fatal("R==B must be rejected — same-UID attach defeats in-memory session secrets")
	}

	// Live attach between same-UID processes (self-parent): often allowed; never
	// treat as isolation proof.
	child := exec.Command(os.Args[0], "-test.run=TestHostileAttachHelper_Sleep", "-test.v")
	child.Env = append(os.Environ(), "HERD_HOSTILE_ATTACH_HELPER=1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _, _ = child.Process.Wait() }()
	time.Sleep(100 * time.Millisecond)
	attachErr := tryAttach(child.Process.Pid)
	if attachErr == nil {
		// Same-UID attach succeeded — documents why topology separates R from B.
		t.Log("same-UID ptrace attach SUCCEEDED (expected on many hosts) — R must != B")
		return
	}
	okDeny, harness := classifyAttachError(attachErr)
	if harness != nil {
		t.Logf("attach harness: %v", harness)
		return
	}
	if okDeny {
		t.Logf("same-UID attach denied by platform policy (%v); still require R!=B topology", attachErr)
	}
}

// TestHostileAttachHelper_Sleep is only started as a child of the attach probe.
func TestHostileAttachHelper_Sleep(t *testing.T) {
	if os.Getenv("HERD_HOSTILE_ATTACH_HELPER") != "1" {
		t.Skip("helper only")
	}
	time.Sleep(5 * time.Second)
}

func hexEncodePayload(p []byte) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(p)*2)
	for i, b := range p {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0x0f]
	}
	return string(out)
}

// hostileWorkerHelperSrc is a minimal dial client (no package imports of herd).
const hostileWorkerHelperSrc = `package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"time"
)

type wireReq struct {
	Op           string ` + "`json:\"op\"`" + `
	CandidateSHA string ` + "`json:\"candidate_sha\"`" + `
	BaseSHA      string ` + "`json:\"base_sha\"`" + `
	PatchID      string ` + "`json:\"patch_id\"`" + `
	Verdict      string ` + "`json:\"verdict\"`" + `
	SessionID    string ` + "`json:\"session_id\"`" + `
	Nonce        string ` + "`json:\"nonce\"`" + `
	PayloadHex   string ` + "`json:\"payload_hex\"`" + `
	MAC          string ` + "`json:\"mac\"`" + `
}

type wireResp struct {
	OK        bool   ` + "`json:\"ok\"`" + `
	Error     string ` + "`json:\"error\"`" + `
	ErrorCode string ` + "`json:\"error_code\"`" + `
}

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: hostile-worker SOCK MAC NONCE")
		os.Exit(1)
	}
	sock, mac, nonce := os.Args[1], os.Args[2], os.Args[3]
	payload := []byte(` + "`" + `{"candidate_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","patch_id":"patch-1","verdict":"APPROVED"}` + "`" + `)
	req := wireReq{
		Op: "sign-verdict",
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PatchID: "patch-1", Verdict: "APPROVED", SessionID: "session-ok",
		Nonce: nonce, PayloadHex: hex.EncodeToString(payload), MAC: mac,
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "encode: %v\n", err)
		os.Exit(1)
	}
	var resp wireResp
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "decode: %v\n", err)
		os.Exit(1)
	}
	if resp.OK {
		fmt.Fprintln(os.Stderr, "ADVERSARIAL_SUCCESS: signature issued to hostile worker")
		os.Exit(0)
	}
	fmt.Fprintf(os.Stderr, "DENIED error_code=%s error=%s\n", resp.ErrorCode, resp.Error)
	if resp.ErrorCode == "UNAUTHORIZED_PEER" {
		os.Exit(2)
	}
	os.Exit(3)
}
`
