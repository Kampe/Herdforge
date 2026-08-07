package signerboundary

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireReady_RejectsTheaterAttestation(t *testing.T) {
	dir := t.TempDir()
	sk, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERD_SIGNER_INSECURE_ENV_SESSION", "1")
	t.Setenv(EnvSessionKey, hex.EncodeToString(sk))
	t.Setenv(EnvSignerUID, "1")
	t.Setenv(EnvRequesterUID, fmt.Sprintf("%d", os.Getuid()))
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", os.Getuid()+1))
	t.Setenv(EnvSocketGID, fmt.Sprintf("%d", os.Getgid()))
	if os.Getuid() == 1 {
		t.Setenv(EnvSignerUID, "2")
	}

	// Otherwise-VALID attestation (correct integrity MAC via writeAttestation,
	// full probe digest, socket set) differing only in mechanism. Without this
	// the test would pass on a missing MAC and could not isolate the mechanism
	// check at all.
	valid := Attestation{
		Mechanism:      MechanismSeparateUID,
		AgentsExcluded: true,
		KeyOwnerUID:    1,
		SocketPath:     "ipc/s.sock",
		ProbeDigest: strings.Join([]string{ProbeKeyUnreadable, ProbeAttachDenied,
			ProbeIPCAuthDenied, ProbeAuthorizedSignOK, ProbePathHardened, ProbeKeyNonExport}, ","),
	}
	if err := writeAttestation(dir, valid, sk); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireReady(dir); err != nil {
		t.Fatalf("control: valid separate-uid attestation must be accepted, got %v", err)
	}

	// writeAttestation refuses theater mechanisms itself, so write the file
	// directly with a correctly recomputed integrity MAC. This isolates
	// RequireReady's own mechanism check: a forged-but-well-signed attestation.
	theater := valid
	theater.Mechanism = "builder-session-sandbox"
	if err := signAttestation(&theater, sk); err != nil {
		t.Fatal(err)
	}
	if err := verifyAttestationMAC(theater, sk); err != nil {
		t.Fatalf("fixture MAC must be valid, else this passes on MAC failure: %v", err)
	}
	raw, err := json.Marshal(theater)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(AttestationFilePath(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(AttestationFilePath(dir)); err != nil {
		t.Fatalf("fixture not at the parsed path: %v", err)
	}
	if _, err := RequireReady(dir); err == nil {
		t.Fatal("fs-sandbox mechanism must fail RequireReady even with a valid MAC")
	}
}

func TestResolveKeyDir_Env(t *testing.T) {
	want := filepath.Join(t.TempDir(), "k")
	t.Setenv(KeyDirEnv, want)
	got, err := ResolveKeyDir()
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%q vs %q", got, want)
	}
}

func TestReady_FalseWithoutBoundary(t *testing.T) {
	dir := t.TempDir()
	if Ready(dir) {
		t.Fatal("Ready must be false with no attestation")
	}
	_, err := RequireReady(dir)
	if err == nil {
		t.Fatal("RequireReady must fail with no attestation")
	}
	if !errors.Is(err, ErrBoundaryNotEstablished) {
		t.Fatalf("want ErrBoundaryNotEstablished, got %v", err)
	}
}
