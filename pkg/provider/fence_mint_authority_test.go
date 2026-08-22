package provider

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestInProcessMinterHasAuthority covers the primary production path.
//
// FAC-564: there was NO production route to a coordinator minter at all, so a
// fenced board write could not complete for any card. Both prior constructors
// refuse by design and stay refused; the boundary here is the address space —
// the mint secret was generated in this process and never written to a file, an
// environment variable, or the wire.
func TestInProcessMinterHasAuthority(t *testing.T) {
	b := startTestBroker(t, "", t.TempDir())
	m, err := CoordinatorMinterInProcess(b)
	if err != nil {
		t.Fatalf("the process owning the broker must be able to mint: %v", err)
	}
	if m == nil || m.mintSecret == "" {
		t.Fatal("minter must carry authority")
	}
	if m.mintSecret != b.mintToken {
		t.Fatal("minter must hold the broker's own mint token")
	}
	// The secret must not be reachable through any exported surface.
	if strings.Contains(m.String(), m.mintSecret) {
		t.Error("String() must not leak the mint secret")
	}
	raw, err := m.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), m.mintSecret) {
		t.Error("JSON must not leak the mint secret")
	}
}

// A process that does not own a broker has no in-process authority.
func TestInProcessMinterRefusesWithoutBroker(t *testing.T) {
	if _, err := CoordinatorMinterInProcess(nil); err == nil {
		t.Fatal("a process without a broker must not mint")
	}
}

// TestWorkerCannotMint is the security property the whole card exists for.
//
// A worker has no inherited descriptor, so every route must refuse: env mint is
// disabled, claim-dir mint is blocked outside tests, and the descriptor path has
// nothing to read.
func TestWorkerCannotMint(t *testing.T) {
	t.Setenv(envFenceMintFD, "")
	if err := os.Unsetenv(envFenceMintFD); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFenceBrokerMinterFromInheritedFD("http://127.0.0.1:1"); err == nil {
		t.Fatal("a worker with no inherited descriptor must not mint")
	}
	if _, err := NewFenceBrokerMinterFromEnv(); err == nil {
		t.Fatal("env mint must stay disabled")
	}
}

// A worker must not be able to forge authority by writing its own secret to a
// file and pointing the variable at it. Only a pipe is accepted.
func TestInheritedMintRefusesNonPipeDescriptor(t *testing.T) {
	forged := filepath.Join(t.TempDir(), "forged.cred")
	if err := os.WriteFile(forged, []byte("aaaaaaaaaaaaaaaaaaaaaaaa\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(forged)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	t.Setenv(envFenceMintFD, strconv.Itoa(int(f.Fd())))
	_, err = NewFenceBrokerMinterFromInheritedFD("http://127.0.0.1:1")
	if err == nil {
		t.Fatal("a regular file must not be accepted as mint authority")
	}
	if !strings.Contains(err.Error(), "pipe") {
		t.Errorf("refusal should name the pipe requirement, got: %v", err)
	}
}

// A descriptor number that was never inherited must fail rather than read
// whatever else happens to sit at that number.
func TestInheritedMintRefusesUninheritedDescriptor(t *testing.T) {
	t.Setenv(envFenceMintFD, "999")
	if _, err := NewFenceBrokerMinterFromInheritedFD("http://127.0.0.1:1"); err == nil {
		t.Fatal("an uninherited descriptor must not grant authority")
	}
	// Descriptors 0-2 are stdio and are never a mint channel.
	for _, fd := range []string{"0", "1", "2", "-1", "notanumber"} {
		t.Setenv(envFenceMintFD, fd)
		if _, err := NewFenceBrokerMinterFromInheritedFD("http://127.0.0.1:1"); err == nil {
			t.Errorf("descriptor %q must not grant authority", fd)
		}
	}
}

// TestGrantMintToChildDeliversOverPipe proves the out-of-process boundary end to
// end: the child reads the secret from an inherited pipe, and the secret appears
// nowhere in the child's environment.
func TestGrantMintToChildDeliversOverPipe(t *testing.T) {
	b := startTestBroker(t, "", t.TempDir())
	// `cat` reads the inherited descriptor; if the pipe works, we see the
	// secret on its stdout and NOT in its environment.
	cmd := exec.Command("sh", "-c", `cat <&"$HERD_FENCE_MINT_FD"; env | grep -c MINT_TOKEN || true`)
	deliver, err := b.GrantMintToChild(cmd)
	if err != nil {
		t.Fatalf("grant: %v", err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deliver()
	buf := make([]byte, 4096)
	n, _ := out.Read(buf)
	_ = cmd.Wait()
	got := string(buf[:n])
	if !strings.Contains(got, b.mintToken) {
		t.Fatalf("child did not receive the secret over the inherited pipe; got %q", got)
	}
	// The descriptor NUMBER may be in the environment; the SECRET may not.
	for _, kv := range cmd.Env {
		if strings.Contains(kv, b.mintToken) {
			t.Fatalf("mint secret leaked into the child environment: %q", kv)
		}
	}
}

// Granting to an already-started child is a programming error, not a silent
// no-op that would leave the child without authority it expects.
func TestGrantMintRefusesStartedChild(t *testing.T) {
	b := startTestBroker(t, "", t.TempDir())
	cmd := exec.Command("true")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Wait() }()
	if _, err := b.GrantMintToChild(cmd); err == nil {
		t.Fatal("cannot grant mint authority to an already-started child")
	}
}

// The mint secret must not be written to the claim dir by the production
// authority path: a mode-0600 file is same-UID readable and is exactly the
// mechanism this card replaces.
func TestProductionWritesNoMintCredentialFile(t *testing.T) {
	b := startTestBroker(t, "", t.TempDir())
	cred := filepath.Join(b.claimDir, mintCredLeaf)
	// Hermetic tests still exercise the claim-dir constructor, so the file
	// exists here. The PRODUCTION branch must remove it and write nothing.
	if _, err := os.Stat(cred); err != nil {
		t.Fatalf("fixture assumption: hermetic brokers still write the credential: %v", err)
	}
	if err := b.reconcileMintCredentialFor(false); err != nil {
		t.Fatalf("production reconcile: %v", err)
	}
	if _, err := os.Stat(cred); !os.IsNotExist(err) {
		t.Error("production must leave no same-UID readable mint credential; a stale one is worse than none")
	}
	// Removing the file must not remove authority: the in-process minter holds
	// the secret in memory, not on disk.
	m, err := CoordinatorMinterInProcess(b)
	if err != nil || m.mintSecret != b.mintToken {
		t.Fatalf("in-process authority must survive with no credential file: %v", err)
	}
	// And a second reconcile is idempotent, not an error on the missing file.
	if err := b.reconcileMintCredentialFor(false); err != nil {
		t.Errorf("reconcile must be idempotent: %v", err)
	}
}


// TestCoordinatorBrokerGrantsMintAuthority covers the wired production path:
// the coordinator hosts the broker and gets authority over it.
func TestCoordinatorBrokerGrantsMintAuthority(t *testing.T) {
	cb, err := StartCoordinatorBroker(CoordinatorBrokerOptions{ClaimDir: t.TempDir(), ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("a coordinator must be able to host its own broker: %v", err)
	}
	defer func() { _ = cb.Close() }()
	if cb.Minter == nil || cb.Minter.mintSecret == "" {
		t.Fatal("coordinator-owned broker must yield mint authority")
	}
	if cb.Client == nil || cb.Client.Token == "" {
		t.Fatal("coordinator needs a worker client for ordinary mutates")
	}
	// The two credentials must differ, or a worker token would mint.
	if cb.Client.Token == cb.Minter.mintSecret {
		t.Fatal("worker token and mint token must never be the same value")
	}
	// Neither credential may be discoverable outside this process.
	if v := os.Getenv(envFenceBrokerMintToken); v != "" {
		t.Errorf("mint token must not be published to the environment, got %q", v)
	}
	if _, err := os.Stat(filepath.Join(cb.Broker.claimDir, mintCredLeaf)); err == nil {
		// Hermetic brokers still write this file; the production reconcile
		// removes it. Assert the coordinator path does not DEPEND on it.
		if err := cb.Broker.reconcileMintCredentialFor(false); err != nil {
			t.Fatal(err)
		}
		if cb.Minter.mintSecret == "" {
			t.Error("authority must not depend on the credential file")
		}
	}
}

// One live broker per claim volume: a second host must be refused rather than
// silently serving a split view of the same fences.
func TestSecondCoordinatorBrokerIsRefused(t *testing.T) {
	dir := t.TempDir()
	first, err := StartCoordinatorBroker(CoordinatorBrokerOptions{ClaimDir: dir, ListenAddr: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if second, err := StartCoordinatorBroker(CoordinatorBrokerOptions{ClaimDir: dir, ListenAddr: "127.0.0.1:0"}); err == nil {
		_ = second.Close()
		t.Fatal("a second broker on one claim volume must be refused")
	}
}

// A worker must never host a broker, because hosting one confers mint
// authority — the exact thing that has to stay impossible.
func TestWorkerRoleCannotOwnBroker(t *testing.T) {
	t.Setenv(envFenceCoordinator, "1")
	for _, role := range []string{"worker", "builder", "reviewer", "WORKER"} {
		t.Setenv("HERD_ROLE", role)
		if coordinatorOwnsBroker() {
			t.Errorf("role %q must not be allowed to host the broker", role)
		}
	}
	t.Setenv("HERD_ROLE", "coordinator")
	if !coordinatorOwnsBroker() {
		t.Error("an explicit coordinator must be allowed to host the broker")
	}
	// Hosting is opt-in: absent the flag, nobody takes the claim-dir lock.
	t.Setenv(envFenceCoordinator, "")
	if coordinatorOwnsBroker() {
		t.Error("hosting must be explicit, never a default")
	}
}

// An over-long claim dir must fail with the real reason. Bare bind errors say
// "invalid argument", which sends an operator looking in the wrong place.
func TestOverLongSocketPathFailsWithTheRealReason(t *testing.T) {
	deep := t.TempDir()
	for i := 0; i < 12; i++ {
		deep = filepath.Join(deep, "aaaaaaaaaaaaaaaaaaaa")
	}
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Skipf("cannot build a long path here: %v", err)
	}
	_, err := StartCoordinatorBroker(CoordinatorBrokerOptions{ClaimDir: deep})
	if err == nil {
		t.Fatal("an over-long unix socket path must be refused")
	}
	if !strings.Contains(err.Error(), "platform limit") {
		t.Errorf("refusal must name the real cause, got: %v", err)
	}
}
