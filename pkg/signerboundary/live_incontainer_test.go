package signerboundary

import (
	"crypto/ed25519"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLiveThreeUID_InContainer is real multi-UID acceptance (no TestPeerUIDOverride).
// Enabled only when HERD_FAC169_IN_DOCKER=1 and herd169{s,r,b} users exist.
func TestLiveThreeUID_InContainer(t *testing.T) {
	if os.Getenv("HERD_FAC169_IN_DOCKER") != "1" {
		t.Skip("not in docker live harness")
	}
	sUID := mustUID(t, "herd169s")
	rUID := mustUID(t, "herd169r")
	bUID := mustUID(t, "herd169b")
	g, err := user.LookupGroup("herd169ipc")
	if err != nil {
		t.Fatal(err)
	}
	gID, _ := strconv.Atoi(g.Gid)

	_ = os.Setenv(EnvSignerUID, itoa(sUID))
	_ = os.Setenv(EnvRequesterUID, itoa(rUID))
	_ = os.Setenv(EnvBuilderUID, itoa(bUID))
	_ = os.Setenv(EnvSocketGID, itoa(gID))

	topo, err := LoadTopology()
	if err != nil {
		t.Fatal(err)
	}

	// World-traversable workspace so S/R/B can reach private/attest/socket.
	root := "/tmp/h169-live-root"
	_ = os.RemoveAll(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	keyDir := filepath.Join(root, "keys")
	if err := EnsureKeyLayout(keyDir, topo); err != nil {
		t.Fatal(err)
	}
	// Ensure keyDir itself is traversable (not 0700 root-only).
	_ = os.Chmod(keyDir, 0o755)
	identity := "live"
	keyPath := PrivateKeyPath(keyDir, identity)
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 9)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(seed)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(keyPath, sUID, gID); err != nil {
		t.Fatal(err)
	}
	_ = os.Chown(filepath.Dir(keyPath), sUID, gID)
	_ = os.Chmod(filepath.Dir(keyPath), 0o700)
	_ = os.Chown(filepath.Join(keyDir, AttestSubdir), rUID, gID)
	_ = os.Chmod(filepath.Join(keyDir, AttestSubdir), 0o770)

	// Build herd from module root into world-executable path.
	modRoot, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	herdBin := filepath.Join(root, "herd")
	// -buildvcs=false: see live_launch_docker_test.go — the bind-mounted repo is
	// "dubious ownership" to git inside the container and Go fails the build.
	build := exec.Command("go", "build", "-buildvcs=false", "-o", herdBin, "./cmd/herd")
	build.Dir = modRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build herd: %v\n%s", err, out)
	}
	_ = os.Chmod(herdBin, 0o755)

	sk, err := NewSessionKey()
	if err != nil {
		t.Fatal(err)
	}
	sock := "/tmp/h169live.sock"
	_ = os.Remove(sock)
	_ = os.Remove(sock + ".nonces")

	ledPath := AdmissionLedgerPath(keyDir)
	if _, err := OpenAdmissionLedger(ledPath); err != nil {
		t.Fatal(err)
	}
	_ = os.Chown(ledPath, rUID, gID)
	_ = os.Chmod(ledPath, 0o660)
	// S must read ledger: group-readable via SocketGID.
	_ = os.Chown(filepath.Dir(ledPath), rUID, gID)

	env := []string{
		EnvSignerUID + "=" + itoa(sUID),
		EnvRequesterUID + "=" + itoa(rUID),
		EnvBuilderUID + "=" + itoa(bUID),
		EnvSocketGID + "=" + itoa(gID),
		"HERD_SIGNER_SESSION_STDIN=1",
		"HERD_ADMISSION_LEDGER=" + ledPath,
		"PATH=" + os.Getenv("PATH"),
		"HOME=/tmp",
	}
	// Start serve as S with session on stdin + durable admission ledger.
	cmd := exec.Command("setpriv",
		"--reuid="+itoa(sUID), "--regid="+itoa(gID), "--init-groups", "--",
		herdBin, "signer-boundary", "serve",
		"--key", keyPath, "--socket", sock, "--session-key-stdin",
		"--admission-ledger", ledPath,
	)
	if _, err := exec.LookPath("setpriv"); err != nil {
		cmd = exec.Command("su", "-s", "/bin/sh", "herd169s", "-c",
			fmt.Sprintf("export %s; %s signer-boundary serve --key %s --socket %s --session-key-stdin --admission-ledger %s",
				strings.Join(env, " "), herdBin, keyPath, sock, ledPath))
	}
	cmd.Env = append(os.Environ(), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve as S: %v", err)
	}
	defer func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }()
	if _, err := stdin.Write([]byte(hex.EncodeToString(sk) + "\n")); err != nil {
		t.Fatal(err)
	}
	_ = stdin.Close()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Lstat(sock); err != nil {
		t.Fatalf("socket not created: %v", err)
	}
	fi, _ := os.Lstat(sock)
	if fi != nil && fi.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode %04o want 0660", fi.Mode().Perm())
	}

	// Client helper (stdlib only) for real R/B processes.
	clientSrc := filepath.Join(root, "client.go")
	if err := os.WriteFile(clientSrc, []byte(liveClientMain), 0o600); err != nil {
		t.Fatal(err)
	}
	clientBin := filepath.Join(root, "client")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", clientBin, clientSrc).CombinedOutput(); err != nil {
		t.Fatalf("client build: %v\n%s", err, out)
	}

	// Write durable admission grants as root then chown to R (FAC-145 channel).
	led, err := OpenAdmissionLedger(ledPath)
	if err != nil {
		t.Fatal(err)
	}
	grant := func(session, nonceSuffix string) {
		t.Helper()
		if err := led.AppendGrant(AdmissionRecord{
			TokenID: "live-" + nonceSuffix, CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PatchID: "p",
			SessionID: session, Verdict: "APPROVED", SingleUse: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	grant("session-ok", "b")
	grant("session-ok", "r")
	// second grant for R replay test will fail at nonce not admission
	grant("session-ok", "r2")

	// Real B key-read denial (strict EACCES/EPERM)
	kout, kerr := runAsUser(t, "herd169b", clientBin, keyPath, "x", "0", "keyread")
	if kerr == nil || strings.Contains(string(kout), "KEY_READ_OK") {
		t.Fatalf("builder key-read must fail: out=%s err=%v", kout, kerr)
	}
	if !strings.Contains(string(kout), "KEY_READ_DENIED") {
		t.Fatalf("builder key-read want KEY_READ_DENIED: %s", kout)
	}

	// Real B: stolen MAC + exact sign-verdict => UNAUTHORIZED_PEER
	vB := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"p", "APPROVED", "session-ok", nil,
	)
	vB.Nonce = "live-b-nonce-001"
	macB := sk.BindRequestMAC(vB)
	bout, err := runAsUser(t, "herd169b", clientBin, sock, macB, vB.Nonce, "sign")
	if err == nil {
		t.Fatalf("builder must fail; out=%s", bout)
	}
	if !strings.Contains(string(bout), "UNAUTHORIZED_PEER") {
		t.Fatalf("builder want UNAUTHORIZED_PEER out=%s err=%v", bout, err)
	}

	// Real R signs (peer UID + durable admission)
	vR := NewVerdictRequest(
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		"p", "APPROVED", "session-ok", nil,
	)
	vR.Nonce = "live-r-nonce-001"
	macR := sk.BindRequestMAC(vR)
	rout, err := runAsUser(t, "herd169r", clientBin, sock, macR, vR.Nonce, "sign")
	if err != nil {
		t.Fatalf("requester must sign: %v out=%s", err, rout)
	}
	if !strings.Contains(string(rout), "SIG_OK") {
		t.Fatalf("requester output: %s", rout)
	}

	// Exact nonce replay denied
	rout2, err := runAsUser(t, "herd169r", clientBin, sock, macR, vR.Nonce, "sign")
	if err == nil || !strings.Contains(string(rout2), "NONCE_REPLAY") {
		t.Fatalf("replay want NONCE_REPLAY out=%s err=%v", rout2, err)
	}

	// B attach: require exact live S pid + EPERM/EACCES (not vacuous non-ATTACH_OK)
	if cmd.Process == nil {
		t.Fatal("serve process missing")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("serve not alive for attach probe: %v", err)
	}
	aout, aerr := runAsUser(t, "herd169b", clientBin, sock, "x", itoa(cmd.Process.Pid), "attach")
	if aerr == nil || strings.Contains(string(aout), "ATTACH_OK") {
		t.Fatalf("builder attach must fail: out=%s err=%v", aout, aerr)
	}
	if !strings.Contains(string(aout), "EPERM") && !strings.Contains(string(aout), "EACCES") {
		t.Fatalf("builder attach want EPERM|EACCES observed: %s", aout)
	}

	fmt.Println("LIVE_OK")
}

func mustUID(t *testing.T, name string) int {
	t.Helper()
	u, err := user.Lookup(name)
	if err != nil {
		t.Fatal(err)
	}
	id, err := strconv.Atoi(u.Uid)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func runAsUser(t *testing.T, username, bin string, args ...string) ([]byte, error) {
	t.Helper()
	// Quote-free args (controlled fixtures only).
	cmd := exec.Command("su", "-s", "/bin/sh", username, "-c",
		bin+" "+strings.Join(args, " "))
	return cmd.CombinedOutput()
}

const liveClientMain = `package main

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) < 5 {
		os.Exit(1)
	}
	mode := os.Args[4]
	if mode == "keyread" {
		path := os.Args[1]
		b, err := os.ReadFile(path)
		if err == nil && len(b) > 0 {
			fmt.Println("KEY_READ_OK")
			os.Exit(0)
		}
		fmt.Printf("KEY_READ_DENIED err=%v\n", err)
		os.Exit(2)
	}
	if mode == "attach" {
		pid, _ := strconv.Atoi(os.Args[3])
		// kill(pid,0): ESRCH = dead (harness fail). EPERM = process exists but
		// not signalable by B — still prove via ptrace; EPERM alone is isolation.
		if err := syscall.Kill(pid, 0); err != nil {
			if err == syscall.ESRCH {
				fmt.Printf("ATTACH_HARNESS target_dead err=%v\n", err)
				os.Exit(3)
			}
			if err == syscall.EPERM {
				// Cross-UID liveness denied — also attempt ptrace for dual signal.
				_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, 16, uintptr(pid), 0, 0, 0, 0)
				if errno == 0 {
					fmt.Println("ATTACH_OK")
					_, _, _ = syscall.Syscall6(syscall.SYS_PTRACE, 17, uintptr(pid), 0, 0, 0, 0)
					os.Exit(0)
				}
				if errno == syscall.EPERM || errno == syscall.EACCES {
					fmt.Printf("ATTACH_DENIED EPERM (kill+ptrace)\n")
					os.Exit(2)
				}
				// kill EPERM + ptrace other: still count kill EPERM as isolation.
				fmt.Println("ATTACH_DENIED EPERM")
				os.Exit(2)
			}
			fmt.Printf("ATTACH_HARNESS kill err=%v\n", err)
			os.Exit(3)
		}
		_, _, errno := syscall.Syscall6(syscall.SYS_PTRACE, 16, uintptr(pid), 0, 0, 0, 0)
		if errno == 0 {
			fmt.Println("ATTACH_OK")
			_, _, _ = syscall.Syscall6(syscall.SYS_PTRACE, 17, uintptr(pid), 0, 0, 0, 0)
			os.Exit(0)
		}
		if errno == syscall.EPERM {
			fmt.Println("ATTACH_DENIED EPERM")
			os.Exit(2)
		}
		if errno == syscall.EACCES {
			fmt.Println("ATTACH_DENIED EACCES")
			os.Exit(2)
		}
		fmt.Printf("ATTACH_HARNESS errno=%v\n", errno)
		os.Exit(3)
	}
	sock, mac, nonce := os.Args[1], os.Args[2], os.Args[3]
	payload := []byte(` + "`" + `{"candidate_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","patch_id":"p","verdict":"APPROVED"}` + "`" + `)
	type wr struct {
		Op           string ` + "`json:\"op\"`" + `
		CandidateSHA string ` + "`json:\"candidate_sha\"`" + `
		BaseSHA      string ` + "`json:\"base_sha\"`" + `
		PatchID      string ` + "`json:\"patch_id\"`" + `
		Verdict      string ` + "`json:\"verdict\"`" + `
		SessionID    string ` + "`json:\"session_id\"`" + `
		Nonce        string ` + "`json:\"nonce\"`" + `
		PayloadHex   string ` + "`json:\"payload_hex\"`" + `
		MAC          string ` + "`json:\"mac\"`" + `
	}
	req := wr{
		Op: "sign-verdict", CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		BaseSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PatchID: "p", Verdict: "APPROVED",
		SessionID: "session-ok", Nonce: nonce, PayloadHex: hex.EncodeToString(payload), MAC: mac,
	}
	conn, err := net.DialTimeout("unix", sock, 3*time.Second)
	if err != nil {
		fmt.Println("dial", err)
		os.Exit(1)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		fmt.Println("encode", err)
		os.Exit(1)
	}
	var resp struct {
		OK        bool   ` + "`json:\"ok\"`" + `
		ErrorCode string ` + "`json:\"error_code\"`" + `
	}
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Println("decode", err)
		os.Exit(1)
	}
	if resp.OK {
		fmt.Println("SIG_OK")
		os.Exit(0)
	}
	fmt.Printf("DENIED error_code=%s\n", resp.ErrorCode)
	os.Exit(2)
}
`
