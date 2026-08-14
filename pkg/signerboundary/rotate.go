package signerboundary

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// RotateKeyResult is the outcome of a transactional key rotation as S.
type RotateKeyResult struct {
	KeyPath      string
	PublicHex    string
	OldRemoved   bool
	NewSignerPID int
	Restarted    bool
}

// RotateKey transactional flow (FAC-169):
//  1. Write new seed to path+".new" (never delete live seed first)
//  2. Stop live serve (SIGTERM/KILL via signer.pid)
//  3. Atomic rename .new → live path
//  4. Publish public key only after new seed is in place
//  5. Caller must restart serve; if RestartServe is set, restart and prove ping
//
// Never publishes public key while old process still holds old key in memory.
func RotateKey(keyDir, identity, repoRoot string, topo Topology, publish bool) (RotateKeyResult, error) {
	return RotateKeyFull(RotateOptions{
		KeyDir: keyDir, Identity: identity, RepoRoot: repoRoot, Topology: topo,
		Publish: publish, RestartServe: false,
	})
}

// RotateOptions configures transactional rotation.
type RotateOptions struct {
	KeyDir          string
	Identity        string
	RepoRoot        string
	Topology        Topology
	Publish         bool
	SocketPath      string
	SessionKey      SessionKey // required if RestartServe
	HerdBinary      string
	RestartServe    bool
	AdmissionLedger string
}

// RotateKeyFull performs rotation with optional serve restart+readback.
func RotateKeyFull(opts RotateOptions) (RotateKeyResult, error) {
	topo := opts.Topology
	if os.Getuid() != topo.SignerUID {
		return RotateKeyResult{}, fmt.Errorf("%w: rotate-key must run as HERD_SIGNER_UID %d (got %d)",
			ErrProvisioning, topo.SignerUID, os.Getuid())
	}
	if err := EnsureKeyLayout(opts.KeyDir, topo); err != nil {
		return RotateKeyResult{}, err
	}
	path := PrivateKeyPath(opts.KeyDir, opts.Identity)
	newPath := path + ".new"

	// 1. Generate into .new first (old seed remains live until stop).
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return RotateKeyResult{}, err
	}
	seed := priv.Seed()
	hexSeed := hex.EncodeToString(seed) + "\n"
	for i := range seed {
		seed[i] = 0
	}
	if err := atomicWriteFile(newPath, []byte(hexSeed), 0o600); err != nil {
		return RotateKeyResult{}, err
	}
	_ = os.Chown(newPath, topo.SignerUID, topo.SocketGID)
	pub := priv.Public().(ed25519.PublicKey)
	for i := range priv {
		priv[i] = 0
	}

	// 2. Stop live signer before swapping seed / publishing.
	sock := opts.SocketPath
	if sock == "" {
		sock = strings.TrimSpace(os.Getenv(EnvSignerSock))
	}
	if err := TerminateSigner(opts.KeyDir, sock); err != nil {
		// If no process, continue (first provision).
		if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no signer pid") {
			// Non-fatal only when process already gone.
			if !strings.Contains(err.Error(), "process already dead") {
				_ = os.Remove(newPath)
				return RotateKeyResult{}, fmt.Errorf("stop live signer before rotate: %w", err)
			}
		}
	}

	// 3. Atomic swap: new → live. Only after serve is stopped.
	if err := os.Rename(newPath, path); err != nil {
		return RotateKeyResult{}, fmt.Errorf("swap new key into place: %w", err)
	}
	if err := auditKeyMaterialPath(path, topo.SignerUID); err != nil {
		return RotateKeyResult{}, err
	}

	pubHex := hex.EncodeToString(pub)
	// 4. Publish only after live seed is the new one and old process is gone.
	if opts.Publish && strings.TrimSpace(opts.RepoRoot) != "" {
		if err := forcePublishPublicKey(opts.RepoRoot, pub); err != nil {
			return RotateKeyResult{}, err
		}
	}

	res := RotateKeyResult{KeyPath: path, PublicHex: pubHex, OldRemoved: true}
	if opts.RestartServe {
		if len(opts.SessionKey) < 16 {
			return res, fmt.Errorf("%w: SessionKey required to restart serve after rotate", ErrProvisioning)
		}
		if sock == "" {
			return res, fmt.Errorf("%w: SocketPath required to restart serve", ErrProvisioning)
		}
		led := opts.AdmissionLedger
		if led == "" {
			led = AdmissionLedgerPath(opts.KeyDir)
		}
		pid, err := restartServe(opts.HerdBinary, path, sock, led, topo, opts.SessionKey)
		if err != nil {
			return res, fmt.Errorf("restart serve after rotate: %w", err)
		}
		res.NewSignerPID = pid
		res.Restarted = true
		_ = atomicWriteFile(SignerPIDPath(opts.KeyDir), []byte(itoa(pid)+"\n"), 0o644)
	}
	return res, nil
}

func forcePublishPublicKey(repoRoot string, pub ed25519.PublicKey) error {
	path := filepath.Join(repoRoot, PublicKeyFile)
	want := hex.EncodeToString(pub) + "\n"
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return atomicWriteFile(path, []byte(want), 0o644)
}

// TerminateSigner signals the live serve process and removes the socket so old
// in-memory authority cannot accept new connections.
func TerminateSigner(keyDir, socketPath string) error {
	pid, err := ReadSignerPID(keyDir)
	if err != nil {
		// Try discover via socket peer.
		if socketPath != "" {
			pid = peerPIDOfSocket(socketPath)
		}
		if pid <= 0 {
			return fmt.Errorf("no signer pid: %w", err)
		}
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	// Alive check
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return fmt.Errorf("process already dead: %w", err)
	}
	_ = proc.Signal(syscall.SIGTERM)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			goto dead
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = proc.Kill()
	time.Sleep(100 * time.Millisecond)
dead:
	if socketPath != "" {
		_ = os.Remove(socketPath)
		_ = os.Remove(socketPath + ".nonces")
	}
	_ = os.Remove(SignerPIDPath(keyDir))
	// Confirm dead.
	if err := proc.Signal(syscall.Signal(0)); err == nil {
		return fmt.Errorf("%w: signer pid %d still alive after kill", ErrProvisioning, pid)
	}
	return nil
}

// RevokeAuthority terminates live serve, wipes durable material, and proves
// the old authority is unusable (no socket, no key if wipeKey, process dead).
func RevokeAuthority(keyDir, identity, socketPath string, wipeKey bool) error {
	var first error
	if err := TerminateSigner(keyDir, socketPath); err != nil && !strings.Contains(err.Error(), "no signer pid") && !strings.Contains(err.Error(), "process already dead") {
		// Continue wipe even if terminate had issues, but record.
		if first == nil {
			first = err
		}
	}
	paths := []string{
		AttestationFilePath(keyDir),
		filepath.Join(keyDir, IsolationAttestFile),
		filepath.Join(keyDir, SessionKeyFile),
		AdmissionLedgerPath(keyDir),
		SignerPIDPath(keyDir),
	}
	if socketPath != "" {
		paths = append(paths, socketPath+".nonces", socketPath)
	}
	if wipeKey && identity != "" {
		paths = append(paths, PrivateKeyPath(keyDir, identity))
		paths = append(paths, filepath.Join(keyDir, identity+KeyFileSuffix))
		paths = append(paths, PrivateKeyPath(keyDir, identity)+".new")
	}
	for _, p := range paths {
		if err := secureRemove(p); err != nil && first == nil {
			first = err
		}
	}
	if _, err := os.Lstat(AttestationFilePath(keyDir)); err == nil {
		return fmt.Errorf("%w: attestation still present", ErrRevoked)
	}
	if wipeKey {
		if _, err := os.Lstat(PrivateKeyPath(keyDir, identity)); err == nil {
			return fmt.Errorf("%w: private key still present", ErrRevoked)
		}
	}
	if socketPath != "" {
		if _, err := os.Lstat(socketPath); err == nil {
			return fmt.Errorf("%w: socket still present", ErrRevoked)
		}
		// Dial must fail.
		req := SignRequest{Op: OpPing, SessionID: "revoke-probe", Nonce: "n"}
		if _, err := dialForErrorCode(socketPath, req, ""); err == nil {
			return fmt.Errorf("%w: dial to revoked socket succeeded", ErrRevoked)
		}
	}
	return first
}

// Boundary.Rotate as S only.
func (b *Boundary) Rotate() error {
	topo, err := LoadTopology()
	if err != nil {
		return err
	}
	if os.Getuid() != topo.SignerUID {
		return fmt.Errorf("%w: rotate must run as signer uid %d via herd signer-boundary rotate-key", ErrProvisioning, topo.SignerUID)
	}
	_, err = RotateKeyFull(RotateOptions{
		KeyDir: b.keyDir, Identity: b.identity, RepoRoot: b.repoRoot, Topology: topo,
		Publish: true, SocketPath: b.socketPath, RestartServe: false,
	})
	return err
}

// Boundary.Revoke durable-wipes and terminates serve when possible.
func (b *Boundary) Revoke() error {
	if b == nil {
		return ErrBoundaryNotEstablished
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	err := RevokeAuthority(b.keyDir, b.identity, b.socketPath, false)
	for i := range b.sessionKey {
		b.sessionKey[i] = 0
	}
	b.sessionKey = nil
	b.pub = nil
	if err != nil {
		return err
	}
	b.revoked = true
	return nil
}

func restartServe(herdBin, keyPath, sock, led string, topo Topology, sk SessionKey) (int, error) {
	if herdBin == "" {
		var err error
		herdBin, err = os.Executable()
		if err != nil {
			return 0, fmt.Errorf("resolve herd executable: %w", err)
		}
	}
	env := append(os.Environ(),
		EnvSignerUID+"="+itoa(topo.SignerUID),
		EnvRequesterUID+"="+itoa(topo.RequesterUID),
		EnvBuilderUID+"="+itoa(topo.BuilderUID),
		EnvSocketGID+"="+itoa(topo.SocketGID),
		"HERD_SIGNER_SESSION_STDIN=1",
		EnvSignerSock+"="+sock,
		"HERD_ADMISSION_LEDGER="+led,
	)
	cmd := exec.Command(herdBin, "signer-boundary", "serve",
		"--key", keyPath, "--socket", sock, "--session-key-stdin",
		"--admission-ledger", led)
	cmd.Env = env
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return 0, err
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	if _, err := stdin.Write([]byte(hex.EncodeToString(sk) + "\n")); err != nil {
		_ = cmd.Process.Kill()
		return 0, err
	}
	_ = stdin.Close()
	// Detach wait
	go func() { _ = cmd.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Lstat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return cmd.Process.Pid, nil
		}
		time.Sleep(40 * time.Millisecond)
	}
	_ = cmd.Process.Kill()
	return 0, fmt.Errorf("restarted serve did not create socket")
}

// ParsePID is a tiny helper.
func ParsePID(s string) (int, error) {
	return strconv.Atoi(strings.TrimSpace(s))
}
