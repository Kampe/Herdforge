package signerboundary

import (
	"os"
	"testing"
)

// FD/env secrets are not an OS boundary when worker UID == requester UID.
// Topology must refuse that configuration; peer-UID policy must deny builders.

func TestSameUIDWorkerTopology_IsRejected(t *testing.T) {
	me := os.Getuid()
	signer := 1
	if me == 1 {
		signer = 2
	}
	t.Setenv(EnvSignerUID, itoa(signer))
	t.Setenv(EnvRequesterUID, itoa(me))
	t.Setenv(EnvBuilderUID, itoa(me)) // hostile: builder is requester
	if _, err := RequireTopology(); err == nil {
		t.Fatal("R==B must be rejected — peer-cred cannot exclude worker")
	}
}

func TestBuilderPeer_DeniedEvenWithValidMACMaterial(t *testing.T) {
	topo := Topology{SignerUID: 10, RequesterUID: 20, BuilderUID: 30}
	// Worker (builder) cannot be authorized by exe-path or MAC; UID check fails first.
	if err := AuthorizePeerUID(topo.BuilderUID, topo); err == nil {
		t.Fatal("builder uid must not be authorized requester")
	}
	if err := AuthorizePeerUID(topo.RequesterUID, topo); err != nil {
		t.Fatal(err)
	}
}

func TestSignVerdict_RequiresCanonicalFields(t *testing.T) {
	b := &Boundary{attest: Attestation{Mechanism: MechanismSeparateUID}}
	_, err := b.SignVerdict("not-a-sha", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "p", "APPROVED", "session-ok", []byte("{}"))
	if err == nil {
		t.Fatal("bad candidate sha must fail")
	}
	req := SignRequest{Op: OpSignVerdict, CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PatchID: "p", Verdict: "APPROVED", SessionID: "session-ok", Payload: []byte("{}")}
	if err := req.ValidateProduction(); err == nil {
		t.Fatal("empty base must fail")
	}
}

func TestRefuseDiskAndBareEnvSession(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/session.mac", []byte("00"), 0o600); err != nil {
		t.Fatal(err)
	}
	// rename to SessionKeyFile
	_ = os.Rename(dir+"/session.mac", dir+"/"+SessionKeyFile)
	if err := refuseSessionKeyOnDisk(dir); err == nil {
		t.Fatal("disk session.mac must be refused")
	}
	t.Setenv(EnvSessionKey, "00112233445566778899aabbccddeeff")
	t.Setenv("HERD_SIGNER_SESSION_KEY_FD", "")
	t.Setenv("HERD_SIGNER_SESSION_STDIN", "")
	t.Setenv("HERD_SIGNER_INSECURE_ENV_SESSION", "")
	if err := refuseInsecureSessionSources(); err == nil {
		t.Fatal("bare env session secret must be refused")
	}
}

func TestLoadServeSessionKey_RejectsArgvHex(t *testing.T) {
	t.Setenv("HERD_SIGNER_INSECURE_ENV_SESSION", "")
	_, err := LoadServeSessionKey(-1, false, "00112233445566778899aabbccddeeff")
	if err == nil {
		t.Fatal("argv hex secret must fail")
	}
}
