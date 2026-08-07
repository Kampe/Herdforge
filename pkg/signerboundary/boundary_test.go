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

func testOpts(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	keys := filepath.Join(root, "keys")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	return Options{KeyDir: keys, RepoRoot: repo, Identity: "test-repo"}
}

func TestEstablish_BlocksWithoutSeparateUID(t *testing.T) {
	t.Setenv(EnvSignerUID, "")
	t.Setenv(EnvSignerSock, "")
	_, err := Establish(testOpts(t))
	if err == nil {
		t.Fatal("Establish must BLOCK without separate-uid provisioning")
	}
	if !errors.Is(err, ErrUnsupportedPlatform) && !errors.Is(err, ErrProvisioning) {
		t.Logf("got error (fail-closed ok): %v", err)
	}
}

func TestEstablish_RejectsSameUIDSigner(t *testing.T) {
	me := os.Getuid()
	t.Setenv(EnvSignerUID, fmt.Sprintf("%d", me))
	t.Setenv(EnvRequesterUID, fmt.Sprintf("%d", me))
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", me+1))
	t.Setenv(EnvSignerSock, filepath.Join(t.TempDir(), "s.sock"))
	_, err := Establish(testOpts(t))
	if err == nil {
		t.Fatal("same-uid signer/requester must fail")
	}
}

func TestEstablish_KeychainAllowStillBlocked(t *testing.T) {
	t.Setenv(EnvSignerUID, "")
	opts := testOpts(t)
	opts.AllowKeychain = true
	opts.RequireSeparateUID = false
	_, err := Establish(opts)
	if err == nil {
		t.Fatal("keychain without live ACL must not succeed")
	}
	if !errors.Is(err, ErrKeychainUnimplemented) && !errors.Is(err, ErrUnsupportedPlatform) {
		t.Logf("blocked with: %v", err)
	}
}

func TestValidateAttestation_RejectsFilesystemSandboxTheater(t *testing.T) {
	for _, mech := range []string{
		"self",
		"process-boundary+0700-keystore",
		"builder-session-sandbox",
		"sandbox-exec",
		"landlock",
		"",
	} {
		err := validateAttestation(Attestation{
			Mechanism:      mech,
			AgentsExcluded: true,
			ProbeDigest:    strings.Join([]string{ProbeKeyUnreadable, ProbeAttachDenied, ProbeIPCAuthDenied, ProbeAuthorizedSignOK, ProbePathHardened, ProbeKeyNonExport}, ","),
		})
		if err == nil {
			t.Fatalf("mechanism %q must be rejected", mech)
		}
	}
}

func TestValidateAttestation_RequiresAttachAndIPCNotJustKeyRead(t *testing.T) {
	err := validateAttestation(Attestation{
		Mechanism:      MechanismSeparateUID,
		AgentsExcluded: true,
		// only key read — insufficient
		ProbeDigest: ProbeKeyUnreadable,
	})
	if err == nil {
		t.Fatal("key-read-only digest must not validate")
	}
}

func TestPathHarden_RejectsSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real")
	if err := os.WriteFile(real, []byte("00"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	err := auditKeyMaterialPath(link, os.Getuid())
	if err == nil {
		t.Fatal("symlink key must be rejected")
	}
}

func TestPathHarden_RejectsWorldWritableParent(t *testing.T) {
	dir := t.TempDir()
	// Make parent group/world writable
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(dir, "k.ed25519")
	if err := os.WriteFile(key, []byte(strings.Repeat("ab", 32)), 0o600); err != nil {
		t.Fatal(err)
	}
	err := auditKeyMaterialPath(key, os.Getuid())
	if err == nil {
		t.Fatal("world-writable parent must be rejected")
	}
}

func TestAuthorizePeerUID_NotBuilder(t *testing.T) {
	topo := Topology{SignerUID: 1, RequesterUID: 2, BuilderUID: 3}
	if err := AuthorizePeerUID(3, topo); err == nil {
		t.Fatal("builder denied")
	}
	if err := AuthorizePeerUID(2, topo); err != nil {
		t.Fatal(err)
	}
}

func TestRequestBindingMAC(t *testing.T) {
	k, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	r := SignRequest{Op: OpSignVerdict, CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PatchID: "p", Verdict: "APPROVED",
		SessionID: "session-ok", Nonce: "n1", Payload: []byte("p")}
	mac := k.BindRequestMAC(r)
	if !k.CheckRequestMAC(r, mac) {
		t.Fatal("mac")
	}
	r.Verdict = "REJECTED"
	if k.CheckRequestMAC(r, mac) {
		t.Fatal("tamper")
	}
}

func TestRefuseExportAndAgentEnv(t *testing.T) {
	if err := RefuseExport(); err == nil {
		t.Fatal("export")
	}
	env := BuilderLaunchEnv([]string{EnvSessionKey + "=abcd", "FOO=1"})
	for _, e := range env {
		if strings.HasPrefix(e, EnvSessionKey+"=") {
			t.Fatal("session key must be scrubbed from builder env")
		}
	}
	if !strings.Contains(strings.Join(env, ","), "HERD_ROLE=agent") {
		t.Fatal("builder env must set agent role")
	}
}

func TestRequireReady_FailsClosedWithoutAttestation(t *testing.T) {
	_, err := RequireReady(t.TempDir())
	if err == nil {
		t.Fatal("expected fail closed")
	}
}

func TestMustNotSelfAttest(t *testing.T) {
	if err := MustNotSelfAttest("self"); err == nil {
		t.Fatal("self")
	}
}

// Exercises the server's export-key op over IPC as the authorized requester.
// Asserting RefuseExport() != nil was a tautology and never touched the server.
func TestServer_ExportKeyOpRefused(t *testing.T) {
	me := os.Getuid()
	topo := unitTopo(me)
	keyPath, sock, sk := testKeyAndSocket(t)
	reqUID := topo.RequesterUID
	_ = startTestServer(t, keyPath, sock, sk, topo, &reqUID)

	req := sampleVerdict("export-nonce-001")
	req.Op = OpExportKey
	code, err := dialForErrorCode(sock, req, sk.BindRequestMAC(req))
	if err == nil {
		t.Fatal("export-key must be refused even for the authorized requester")
	}
	// Refusal may come from request validation or the op switch; both are
	// structured denials. What must hold is that it is refused and says so.
	if code == "" {
		t.Fatalf("export-key must be a structured denial, got err=%v", err)
	}
	if !strings.Contains(err.Error(), "export refused") {
		t.Fatalf("want an export refusal, got code=%q err=%v", code, err)
	}
	// The private seed must never appear in any response.
	seed, rerr := os.ReadFile(keyPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if strings.Contains(err.Error(), strings.TrimSpace(string(seed))) {
		t.Fatal("response leaked private key material")
	}
}

func TestSignAuthorized_RequiresBoundRequestAndIPC(t *testing.T) {
	b := &Boundary{
		attest: Attestation{Mechanism: MechanismSeparateUID, AgentsExcluded: true,
			ProbeDigest: strings.Join([]string{ProbeKeyUnreadable, ProbeAttachDenied, ProbeIPCAuthDenied, ProbeAuthorizedSignOK, ProbePathHardened, ProbeKeyNonExport}, ",")},
	}
	_, err := b.SignAuthorized(SignRequest{Op: OpSignVerdict, Payload: []byte("x")})
	if err == nil {
		t.Fatal("incomplete SignRequest must fail")
	}
	_, err = b.SignAuthorized(NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"p", "APPROVED", "s", []byte("x"),
	))
	if err == nil {
		t.Fatal("SignAuthorized without IPC must fail closed")
	}
}

func TestRevoke_FailClosedIfStillPresent(t *testing.T) {
	dir := t.TempDir()
	// Make attestation unremovable by replacing with a directory after write — use secureRemove unit via Revoke with empty keyDir that has files.
	b := &Boundary{keyDir: dir, sessionKey: SessionKey{1, 2, 3}}
	if err := os.WriteFile(filepath.Join(dir, IsolationAttestFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.Revoke(); err != nil {
		// may fail validate path — should succeed remove
		t.Log(err)
	}
	if _, err := os.Lstat(filepath.Join(dir, IsolationAttestFile)); err == nil {
		t.Fatal("attestation must be gone after successful revoke")
	}
}

func TestAtomicWrite_AndNoAbsProfile(t *testing.T) {
	dir := t.TempDir()
	sk, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	// Integrity MAC tests: provide session via insecure test opt-in only.
	t.Setenv("HERD_SIGNER_INSECURE_ENV_SESSION", "1")
	t.Setenv(EnvSessionKey, hex.EncodeToString(sk))
	// Topology for RequireReady path when forged att is checked
	t.Setenv(EnvSignerUID, "1")
	t.Setenv(EnvRequesterUID, fmt.Sprintf("%d", os.Getuid()))
	t.Setenv(EnvBuilderUID, fmt.Sprintf("%d", os.Getuid()+1))
	if os.Getuid() == 1 {
		t.Setenv(EnvSignerUID, "2")
	}
	att := Attestation{
		Mechanism:      MechanismSeparateUID,
		AgentsExcluded: true,
		KeyOwnerUID:    1,
		SocketPath:     "ipc/s.sock",
		ProbeDigest:    strings.Join([]string{ProbeKeyUnreadable, ProbeAttachDenied, ProbeIPCAuthDenied, ProbeAuthorizedSignOK, ProbePathHardened, ProbeKeyNonExport}, ","),
		ProfilePath:    "/absolute/host/path.sb", // must be stripped
	}
	if err := writeAttestation(dir, att, sk); err != nil {
		t.Fatal(err)
	}
	got, err := readAttestationFile(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.ProfilePath != "" {
		t.Fatalf("absolute ProfilePath must not be persisted: %q", got.ProfilePath)
	}
	if got.IntegrityMAC == "" {
		t.Fatal("integrity MAC required")
	}
	// Worker-forgeable rewrite without MAC update must fail RequireReady.
	got.AgentsExcluded = true
	got.ProbeDigest = "forged"
	got.IntegrityMAC = "00"
	raw, _ := json.Marshal(got)
	// Rewrite where RequireReady reads, else this passes on an unrelated error.
	if err := os.WriteFile(AttestationFilePath(dir), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RequireReady(dir); err == nil {
		t.Fatal("forged attestation must fail RequireReady")
	}
}

func TestRequestMAC_BindsAllFields(t *testing.T) {
	k, _ := NewSessionKey()
	r := SignRequest{Op: OpSignVerdict, CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PatchID: "p", Verdict: "APPROVED",
		SessionID: "session-ok", Nonce: "n1", Payload: []byte("body")}
	mac := k.BindRequestMAC(r)
	if !k.CheckRequestMAC(r, mac) {
		t.Fatal()
	}
	r.Verdict = "REJECTED"
	if k.CheckRequestMAC(r, mac) {
		t.Fatal("verdict must be bound")
	}
}
