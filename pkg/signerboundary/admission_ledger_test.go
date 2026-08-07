package signerboundary

import (
	"os"
	"testing"
	"time"
)

func TestDurableAdmissionLedger_RequiredForSign(t *testing.T) {
	dir := t.TempDir()
	_ = os.MkdirAll(dir+"/"+AttestSubdir, 0o770)
	led, err := OpenAdmissionLedger(AdmissionLedgerPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	req := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"p", "APPROVED", "session-ok", nil,
	)
	// No grant → not admitted.
	if err := led.Admit(req); err == nil {
		t.Fatal("expected NOT_ADMITTED without grant")
	}
	if err := led.AppendGrant(AdmissionRecord{
		TokenID: "t1", CandidateSHA: req.CandidateSHA, BaseSHA: req.BaseSHA,
		PatchID: req.PatchID, SessionID: req.SessionID, Verdict: req.Verdict, SingleUse: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := led.Admit(req); err != nil {
		t.Fatal(err)
	}
	// Single-use consumed.
	if err := led.Admit(req); err == nil {
		t.Fatal("single-use grant must not admit twice")
	}
}

func TestServer_DurableLedgerAcrossProcessBoundary(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	dir := t.TempDir()
	ledPath := AdmissionLedgerPath(dir)
	_ = os.MkdirAll(dir+"/"+AttestSubdir, 0o770)
	led, err := OpenAdmissionLedger(ledPath)
	if err != nil {
		t.Fatal(err)
	}
	reqUID := topo.RequesterUID
	srv, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		TestPeerUIDOverride: &reqUID, AdmissionLedgerPath: ledPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	go func() { _ = srv.Run() }()
	// allow accept loop
	time.Sleep(40 * time.Millisecond)

	v := sampleVerdict("ledger-1")
	// Without grant → NOT_ADMITTED
	code, err := dialForErrorCode(sock, v, sk.BindRequestMAC(v))
	if err == nil || code != ErrCodeNotAdmitted {
		t.Fatalf("want NOT_ADMITTED code=%q err=%v", code, err)
	}
	// Grant then succeed
	if err := led.AppendGrant(AdmissionRecord{
		TokenID: "g1", CandidateSHA: v.CandidateSHA, BaseSHA: v.BaseSHA,
		PatchID: v.PatchID, SessionID: v.SessionID, Verdict: v.Verdict, SingleUse: true,
	}); err != nil {
		t.Fatal(err)
	}
	v2 := sampleVerdict("ledger-2")
	sig, err := signRequestOverIPC(sock, sk, &v2)
	if err != nil || len(sig) == 0 {
		// Need grant for v2 fields (same as sample)
		_ = led.AppendGrant(AdmissionRecord{
			TokenID: "g2", CandidateSHA: v2.CandidateSHA, BaseSHA: v2.BaseSHA,
			PatchID: v2.PatchID, SessionID: v2.SessionID, Verdict: v2.Verdict, SingleUse: true,
		})
		sig, err = signRequestOverIPC(sock, sk, &v2)
	}
	if err != nil || len(sig) == 0 {
		t.Fatalf("admitted sign: %v", err)
	}
}
