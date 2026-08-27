package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FAC-592: `herd status` printed the bare dial error and stopped, while
// dispatch's requireServingBroker named `herd broker ensure` in its refusal.
// Status is the surface an operator reads first, so the dead end was on the
// wrong one.
//
// This exercises brokerFailureDetail, which is what readBrokerHealth assigns to
// the Detail field that both `herd status` call sites print verbatim.

func TestStaleSocketIsReportedAsAbandonedNotMerelyRefused(t *testing.T) {
	// The real incident: a socket file left behind by a coordinator that died
	// seven days earlier. The file existing is NOT evidence of a listener, and
	// the bare dial error gives an operator no way to tell the difference.
	dir := t.TempDir()
	sock := filepath.Join(dir, "herd-test.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	got := brokerFailureDetail(sock, errors.New("connect: connection refused"))

	if !strings.Contains(got, "herd broker ensure") {
		t.Fatalf("refusal does not name the recovery an operator should run:\n%s", got)
	}
	if !strings.Contains(got, "nothing is listening") {
		t.Fatalf("refusal does not say the socket is abandoned, so the file reads as proof of a live broker:\n%s", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("refusal dropped the underlying dial error, which is the evidence:\n%s", got)
	}
}

func TestMissingSocketIsDistinguishedFromAnAbandonedOne(t *testing.T) {
	// Same remedy, different story: never-started versus died-and-left-a-file.
	// Collapsing them hides whether something crashed.
	got := brokerFailureDetail(filepath.Join(t.TempDir(), "absent.sock"), errors.New("connect: no such file or directory"))

	if !strings.Contains(got, "herd broker ensure") {
		t.Fatalf("refusal does not name the recovery:\n%s", got)
	}
	if !strings.Contains(got, "no socket file") {
		t.Fatalf("refusal does not distinguish a missing socket from an abandoned one:\n%s", got)
	}
	if strings.Contains(got, "nothing is listening") {
		t.Fatalf("a missing socket was described as an abandoned one:\n%s", got)
	}
}

// The two cases must not produce the same sentence, or the distinction the
// operator needs is lost even though both strings mention the remedy.
func TestTheTwoFailuresDoNotReadIdentically(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "stale.sock")
	if err := os.WriteFile(stale, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := errors.New("connect: connection refused")
	if brokerFailureDetail(stale, err) == brokerFailureDetail(filepath.Join(dir, "absent.sock"), err) {
		t.Fatal("an abandoned socket and a missing one produced the same detail")
	}
}
