package signerboundary

import (
	"crypto/ed25519"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Protocol unit tests may inject peer UID. Production and live e2e leave
// TestPeerUIDOverride nil (SO_PEERCRED only).

func testKeyAndSocket(t *testing.T) (keyPath, sock string, sk SessionKey) {
	t.Helper()
	dir := t.TempDir()
	_ = os.Chmod(dir, 0o700)
	priv := filepath.Join(dir, PrivateSubdir)
	_ = os.MkdirAll(priv, 0o700)
	keyPath = filepath.Join(priv, "k.ed25519")
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 3)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var err error
	sk, err = NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	sock, err = shortSocketPath("itest")
	if err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")
	return keyPath, sock, sk
}

func unitTopo(me int) Topology {
	return Topology{
		SignerUID:    me,
		RequesterUID: me + 100001,
		BuilderUID:   me + 200002,
		SocketGID:    os.Getgid(),
	}
}

func startTestServer(t *testing.T, keyPath, sock string, sk SessionKey, topo Topology, peer *int) *Server {
	t.Helper()
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")
	srv, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		TestPeerUIDOverride: peer, NonceLedgerPath: sock + ".nonces",
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Run() }()
	time.Sleep(40 * time.Millisecond)
	t.Cleanup(func() { _ = srv.Close(); _ = os.Remove(sock); _ = os.Remove(sock + ".nonces") })
	return srv
}

func sampleVerdict(nonce string) SignRequest {
	r := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"patch-1", "APPROVED", "session-ok", nil,
	)
	r.Nonce = nonce
	return r
}

func TestServer_SocketACL_Not0600(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	srv, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	fi, err := os.Lstat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %04o want 0660 (0600 blocks R)", fi.Mode().Perm())
	}
}

func TestServer_BuilderPeerDenied_RequesterPeerSigns(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)

	// Unit-only injected builder peer.
	builder := topo.BuilderUID
	_ = startTestServer(t, keyPath, sock, sk, topo, &builder)
	vreq := sampleVerdict("n-builder-hostile-001")
	code, err := dialForErrorCode(sock, vreq, sk.BindRequestMAC(vreq))
	if err == nil || code != ErrCodeUnauthorizedPeer {
		t.Fatalf("builder peer must be UNAUTHORIZED_PEER: code=%q err=%v", code, err)
	}

	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")
	reqUID := topo.RequesterUID
	_ = startTestServer(t, keyPath, sock, sk, topo, &reqUID)
	vreq2 := sampleVerdict("n-requester-ok-001")
	sig, err := signRequestOverIPC(sock, sk, &vreq2)
	if err != nil || len(sig) == 0 {
		t.Fatalf("requester sign-verdict: %v", err)
	}

	mac := sk.BindRequestMAC(vreq2)
	code, err = dialForErrorCode(sock, vreq2, mac)
	if code != ErrCodeReplay {
		t.Fatalf("want NONCE_REPLAY got %q err=%v", code, err)
	}
}

func TestServer_UnadmittedVerdict_Rejected(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	reqUID := topo.RequesterUID
	_ = startTestServer(t, keyPath, sock, sk, topo, &reqUID)

	// Syntactically valid fields but payload does not bind candidate/verdict.
	req := SignRequest{
		Op:           OpSignVerdict,
		CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		PatchID:      "p",
		Verdict:      "APPROVED",
		SessionID:    "session-ok",
		Nonce:        "unadmitted-001",
		Payload:      []byte(`{"unrelated":true}`),
	}
	code, err := dialForErrorCode(sock, req, sk.BindRequestMAC(req))
	if err == nil || code != ErrCodeNotAdmitted {
		t.Fatalf("want NOT_ADMITTED got code=%q err=%v", code, err)
	}
}

func TestServer_RealPeerCreds_CurrentUIDIsSigner_Denied(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	srv, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() { _ = srv.Run() }()
	time.Sleep(40 * time.Millisecond)

	vreq := sampleVerdict("real-peer-signer-denied")
	code, err := dialForErrorCode(sock, vreq, sk.BindRequestMAC(vreq))
	if err == nil || code != ErrCodeUnauthorizedPeer {
		t.Fatalf("real peer uid=signer must be UNAUTHORIZED_PEER: code=%q err=%v", code, err)
	}
}

func TestServer_RestartDurableReplay(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	reqUID := topo.RequesterUID
	srv := startTestServer(t, keyPath, sock, sk, topo, &reqUID)

	vreq := sampleVerdict("fixed-nonce-restart-001")
	if _, err := signRequestOverIPC(sock, sk, &vreq); err != nil {
		t.Fatal(err)
	}
	_ = srv.Close()
	time.Sleep(20 * time.Millisecond)

	srv2, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		TestPeerUIDOverride: &reqUID, NonceLedgerPath: sock + ".nonces",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv2.Close()
	go func() { _ = srv2.Run() }()
	time.Sleep(40 * time.Millisecond)

	mac := sk.BindRequestMAC(vreq)
	code, err := dialForErrorCode(sock, vreq, mac)
	if code != ErrCodeReplay {
		t.Fatalf("after restart want NONCE_REPLAY got %q err=%v", code, err)
	}
}

func TestHostileWorker_SameUIDAsRequester_TopologyBlocks(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvSignerUID, "1")
	if me == 1 {
		t.Setenv(EnvSignerUID, "2")
	}
	t.Setenv(EnvRequesterUID, itoa(me))
	t.Setenv(EnvBuilderUID, itoa(me))
	t.Setenv(EnvSocketGID, itoa(os.Getgid()))
	if _, err := RequireTopology(); err == nil {
		t.Fatal("must refuse R==B")
	}
	if _, err := LoadTopology(); err == nil {
		t.Fatal("LoadTopology must refuse R==B")
	}
}

func TestLoadTopology_RequiresSocketGID(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvSignerUID, "1")
	t.Setenv(EnvRequesterUID, "2")
	t.Setenv(EnvBuilderUID, "3")
	t.Setenv(EnvSocketGID, "")
	if _, err := LoadTopology(); err == nil {
		t.Fatal("missing SOCK_GID must BLOCK")
	}
	t.Setenv(EnvSocketGID, itoa(os.Getgid()))
	if me == 1 || me == 2 || me == 3 {
		t.Setenv(EnvSignerUID, itoa(me+10))
		t.Setenv(EnvRequesterUID, itoa(me+11))
		t.Setenv(EnvBuilderUID, itoa(me+12))
	}
	if _, err := LoadTopology(); err != nil {
		t.Fatal(err)
	}
}
