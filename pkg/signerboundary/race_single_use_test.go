package signerboundary

import (
	"encoding/hex"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// waitForSocket blocks until the accept loop has bound the socket, so a slow
// start cannot be mistaken for a denial.
func waitForSocket(t *testing.T, sock string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("signer socket %s not ready", sock)
}

// Acceptance criterion: race tests prove the single-use guarantees hold.
//
// Both ledgers gate signature issuance. If the admission flock or the nonce
// mutex is removed, concurrent requests lose updates and one approved reviewer
// verdict yields several valid signatures — a builder-reachable replay of
// signing authority. Sequential replay tests cannot catch that.

func raceServer(t *testing.T) (sock string, sk SessionKey, led *DurableAdmissionLedger) {
	t.Helper()
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	dir := t.TempDir()
	ledPath := AdmissionLedgerPath(dir)
	if err := os.MkdirAll(dir+"/"+AttestSubdir, 0o770); err != nil {
		t.Fatal(err)
	}
	led, err := OpenAdmissionLedger(ledPath)
	if err != nil {
		t.Fatal(err)
	}
	reqUID := topo.RequesterUID
	srv, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk, Topology: topo,
		TestPeerUIDOverride: &reqUID, AdmissionLedgerPath: ledPath,
		NonceLedgerPath: sock + ".nonces",
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Run() }()
	t.Cleanup(func() {
		_ = srv.Close()
		_ = os.Remove(sock)
		_ = os.Remove(sock + ".nonces")
	})
	waitForSocket(t, sock)
	return sock, sk, led
}

// grantFor appends a single-use grant matching sampleVerdict's bound fields.
func grantFor(t *testing.T, led *DurableAdmissionLedger, token string) {
	t.Helper()
	v := sampleVerdict("unused")
	if err := led.AppendGrant(AdmissionRecord{
		TokenID: token, CandidateSHA: v.CandidateSHA, BaseSHA: v.BaseSHA,
		PatchID: v.PatchID, SessionID: v.SessionID, Verdict: v.Verdict,
		SingleUse: true,
	}); err != nil {
		t.Fatal(err)
	}
}

// signOnce sends one fully-formed request, mirroring what the real client puts
// on the wire (PayloadHex populated) so a denial means policy, not a malformed
// fixture.
func signOnce(sock string, v SignRequest, mac string) ([]byte, error) {
	if len(v.Payload) > 0 {
		v.PayloadHex = hex.EncodeToString(v.Payload)
	}
	return signRequestOverIPCWithMAC(sock, v, mac)
}

// fanOut fires n concurrent signing attempts released from a common barrier and
// returns how many produced a signature.
func fanOut(t *testing.T, n int, attempt func(i int) ([]byte, error)) int {
	t.Helper()
	var wg sync.WaitGroup
	sigs := make([][]byte, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			sigs[i], errs[i] = attempt(i)
		}(i)
	}
	close(start)
	wg.Wait()
	signed := 0
	for i := range sigs {
		if errs[i] == nil && len(sigs[i]) > 0 {
			signed++
		}
	}
	return signed
}

// One single-use grant, concurrent requests with distinct nonces: the nonce
// ledger cannot be what limits the result, so this isolates admission.
func TestRace_SingleUseAdmission_SignsExactlyOnce(t *testing.T) {
	sock, sk, led := raceServer(t)
	grantFor(t, led, "race-grant-1")

	signed := fanOut(t, 8, func(i int) ([]byte, error) {
		v := sampleVerdict(fmt.Sprintf("race-adm-nonce-%d", i))
		return signOnce(sock, v, sk.BindRequestMAC(v))
	})
	if signed != 1 {
		t.Fatalf("single-use admission grant signed %d times under concurrency, want exactly 1", signed)
	}
}

// Enough grants that admission cannot be the limiter, all sharing one nonce:
// this isolates the durable nonce ledger.
func TestRace_SingleUseNonce_SignsExactlyOnce(t *testing.T) {
	sock, sk, led := raceServer(t)
	const n = 8
	for i := 0; i < n; i++ {
		grantFor(t, led, fmt.Sprintf("race-grant-%d", i))
	}

	v := sampleVerdict("race-shared-nonce-001")
	mac := sk.BindRequestMAC(v)
	signed := fanOut(t, n, func(int) ([]byte, error) {
		return signOnce(sock, v, mac)
	})
	if signed != 1 {
		t.Fatalf("shared nonce signed %d times under concurrency, want exactly 1", signed)
	}
}
