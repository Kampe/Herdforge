package signerboundary

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
)

// Causal mutation guards: regressions that re-introduce soft-open, 0600 sockets,
// or R==B topology must fail these tests.

func TestMutation_RequireLaunchReady_NotSoftOpen(t *testing.T) {
	t.Setenv(EnvSignerUID, "")
	t.Setenv(EnvRequesterUID, "")
	t.Setenv(EnvBuilderUID, "")
	t.Setenv(EnvSocketGID, "")
	if err := RequireLaunchReady(t.TempDir()); err == nil {
		t.Fatal("unprovisioned launch must fail closed")
	}
}

func TestMutation_AuthorizePeerUID_BuilderNeverOK(t *testing.T) {
	topo := Topology{SignerUID: 1, RequesterUID: 2, BuilderUID: 3, SocketGID: 4}
	if err := AuthorizePeerUID(3, topo); err == nil {
		t.Fatal("builder must not authorize")
	}
}

func TestMutation_LoadTopology_RejectsMissingSockGID(t *testing.T) {
	t.Setenv(EnvSignerUID, "10")
	t.Setenv(EnvRequesterUID, "11")
	t.Setenv(EnvBuilderUID, "12")
	t.Setenv(EnvSocketGID, "")
	_, err := LoadTopology()
	if err == nil || !strings.Contains(err.Error(), "SOCK_GID") && !strings.Contains(err.Error(), "GID") {
		t.Fatalf("want SOCK_GID blocked error, got %v", err)
	}
}

func TestMutation_StartServer_RejectsZeroSocketGID(t *testing.T) {
	me := os.Getuid()
	keyPath, sock, sk := testKeyAndSocket(t)
	_, err := StartServer(ServeOptions{
		KeyPath: keyPath, SocketPath: sock, SessionKey: sk,
		Topology: Topology{SignerUID: me, RequesterUID: me + 1, BuilderUID: me + 2, SocketGID: 0},
	})
	if err == nil {
		t.Fatal("SocketGID=0 must fail")
	}
}

// The provision path (Establish -> loadOrCreateSessionKey) refuses same-UID
// readable session material. The fail-closed launch gate must refuse it too,
// or the invariant is enforced on one path and dropped on the other.
//
// The attestation below is deliberately VALID so RequireReady passes: otherwise
// the gate would fail on a missing attestation and this would assert nothing.
func TestMutation_LaunchGate_RefusesInsecureSessionSource(t *testing.T) {
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
	att := Attestation{
		Mechanism:      MechanismSeparateUID,
		AgentsExcluded: true,
		KeyOwnerUID:    1,
		SocketPath:     "ipc/s.sock",
		ProbeDigest: strings.Join([]string{ProbeKeyUnreadable, ProbeAttachDenied,
			ProbeIPCAuthDenied, ProbeAuthorizedSignOK, ProbePathHardened, ProbeKeyNonExport}, ","),
	}
	if err := writeAttestation(dir, att, sk); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireReady(dir); err != nil {
		t.Fatalf("control: attestation must be ready, else the gate is untested: %v", err)
	}
	t.Setenv(KeyDirEnv, dir)

	err = RequireLaunchReady(dir)
	if err == nil {
		t.Fatal("launch gate must refuse an insecure session source")
	}
	// Must fail on the session source specifically, not on a live-dial error.
	if !strings.Contains(err.Error(), "HERD_SIGNER_INSECURE_ENV_SESSION") {
		t.Fatalf("want refusal of the insecure session source, got %v", err)
	}
}
