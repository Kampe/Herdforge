package signerboundary

import (
	"testing"
)

func TestEncodeProbeDigest_RequiresErrnoMatch(t *testing.T) {
	base := []ProbeReceipt{
		{Version: 1, Platform: "test", Operation: "path-harden", OK: true},
		{Version: 1, Platform: "test", Operation: "key-read", OK: true, ExpectedErrno: "EACCES|EPERM", ObservedErrno: "EPERM"},
		{Version: 1, Platform: "test", Operation: "key-non-export", OK: true},
		{Version: 1, Platform: "test", Operation: "ipc-unauth", OK: true},
		{Version: 1, Platform: "test", Operation: "ipc-auth", OK: true},
		{Version: 1, Platform: "test", Operation: "attach", OK: true, ExpectedErrno: "EPERM|EACCES", ObservedErrno: "EPERM", SignerPID: 1, SignerUID: 2},
	}
	if _, err := EncodeProbeDigest(base); err != nil {
		t.Fatal(err)
	}
	// ENOENT observed while expecting EPERM must BLOCK
	bad := append([]ProbeReceipt{}, base...)
	bad[1].ObservedErrno = "ENOENT no such file"
	if _, err := EncodeProbeDigest(bad); err == nil {
		t.Fatal("ENOENT must not count as EPERM denial")
	}
	// attach missing pid/uid BLOCK
	bad2 := append([]ProbeReceipt{}, base...)
	bad2[5].SignerPID = 0
	if _, err := EncodeProbeDigest(bad2); err == nil {
		t.Fatal("attach without live pid must fail")
	}
}

func TestDurableNonce_ExactReplay(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/nonces"
	l, err := NewDurableNonceLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Accept("n1") {
		t.Fatal("first accept")
	}
	if l.Accept("n1") {
		t.Fatal("second accept must fail")
	}
	// Reload from disk
	l2, err := NewDurableNonceLedger(path)
	if err != nil {
		t.Fatal(err)
	}
	if l2.Accept("n1") {
		t.Fatal("reload must still reject n1")
	}
	if !l2.Accept("n2") {
		t.Fatal("new nonce ok")
	}
}
