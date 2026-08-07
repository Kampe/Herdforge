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

// LaunchConfig provisions key layout, starts serve as S, seals session for R,
// live-proves with real RunAs(B)/RunAs(R) probes, and returns without blocking
// on serve Wait (supervisor lifecycle is separate).
type LaunchConfig struct {
	KeyDir     string
	RepoRoot   string
	Identity   string
	SocketPath string
	HerdBinary string
	RunAs      func(uid int, env []string, name string, args ...string) *exec.Cmd
	SkipServe  bool
	// SkipLiveProve is unit-test only; production launch must prove.
	SkipLiveProve bool
	// DetachServe when true (default) does not wait on serve exit.
	DetachServe bool
}

// LaunchHandle is the post-launch supervisor state. SessionKey is wiped after
// sealing to R-owned session.rkey — use SealedSessionPath for repeated R use.
type LaunchHandle struct {
	Topo          Topology
	SocketPath    string
	KeyPath       string
	KeyDir        string
	Identity      string
	SignerPID     int
	LedgerPath    string
	SealedSession string
	cmd           *exec.Cmd
	waitErr       chan error
}

// Cmd returns the serve process (may be nil after detach bookkeeping).
func (h *LaunchHandle) Cmd() *exec.Cmd {
	if h == nil {
		return nil
	}
	return h.cmd
}

// WaitErr is optional; production launch detaches and does not require Wait.
func (h *LaunchHandle) WaitErr() <-chan error {
	if h == nil {
		return nil
	}
	return h.waitErr
}

// Close stops serve if still tracked.
func (h *LaunchHandle) Close() error {
	if h == nil || h.cmd == nil || h.cmd.Process == nil {
		return nil
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-h.waitErr:
	case <-time.After(3 * time.Second):
		_ = h.cmd.Process.Kill()
		select {
		case <-h.waitErr:
		case <-time.After(2 * time.Second):
		}
	}
	return nil
}

// ProvisionAndLaunch creates keys, starts S, seals session for R, live-proves
// with real B/R RunAs, and returns. Does not print secrets or block on Wait.
func ProvisionAndLaunch(cfg LaunchConfig) (*LaunchHandle, error) {
	topo, err := LoadTopology()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(cfg.KeyDir) == "" || strings.TrimSpace(cfg.Identity) == "" {
		return nil, fmt.Errorf("%w: KeyDir and Identity required", ErrProvisioning)
	}
	if strings.TrimSpace(cfg.SocketPath) == "" {
		return nil, fmt.Errorf("%w: SocketPath required", ErrProvisioning)
	}
	runAs := cfg.RunAs
	if runAs == nil {
		runAs = defaultRunAs
	}

	if err := EnsureKeyLayout(cfg.KeyDir, topo); err != nil {
		return nil, err
	}
	// Apply shared ACL on attest/ for R+S ledger access.
	_ = os.Chown(filepath.Join(cfg.KeyDir, AttestSubdir), topo.RequesterUID, topo.SocketGID)
	_ = os.Chmod(filepath.Join(cfg.KeyDir, AttestSubdir), 0o770)

	keyPath := PrivateKeyPath(cfg.KeyDir, cfg.Identity)
	if _, err := os.Lstat(keyPath); err != nil {
		if err := generateInitialKey(keyPath, topo); err != nil {
			return nil, err
		}
	}
	if err := auditKeyMaterialPath(keyPath, topo.SignerUID); err != nil {
		return nil, fmt.Errorf("%w: key layout: %v", ErrProvisioning, err)
	}

	// Publish public key without root reading private seed: derive while we can
	// open as owner, or via S run-as export of public only.
	if strings.TrimSpace(cfg.RepoRoot) != "" {
		if err := publishPublicFromKey(keyPath, cfg.RepoRoot, topo, runAs, cfg.HerdBinary); err != nil {
			return nil, err
		}
	}

	sk, err := NewSessionKey()
	if err != nil {
		return nil, err
	}
	// Seal for repeated R use BEFORE wiping handle copy after handoff to S.
	if err := WriteSealedSession(cfg.KeyDir, topo, sk); err != nil {
		return nil, err
	}

	ledPath := AdmissionLedgerPath(cfg.KeyDir)
	if _, err := OpenAdmissionLedgerTopo(ledPath, topo); err != nil {
		return nil, err
	}

	h := &LaunchHandle{
		Topo:          topo,
		SocketPath:    cfg.SocketPath,
		KeyPath:       keyPath,
		KeyDir:        cfg.KeyDir,
		Identity:      cfg.Identity,
		LedgerPath:    ledPath,
		SealedSession: SealedSessionPath(cfg.KeyDir),
		waitErr:       make(chan error, 1),
	}
	if cfg.SkipServe {
		return h, nil
	}

	bin := cfg.HerdBinary
	if bin == "" {
		bin, err = exec.LookPath("herd")
		if err != nil {
			bin = os.Args[0]
		}
	}

	hexKey := hex.EncodeToString(sk) + "\n"
	env := filterEnv(os.Environ(), EnvSessionKey)
	env = append(env,
		EnvSignerUID+"="+itoa(topo.SignerUID),
		EnvRequesterUID+"="+itoa(topo.RequesterUID),
		EnvBuilderUID+"="+itoa(topo.BuilderUID),
		EnvSocketGID+"="+itoa(topo.SocketGID),
		"HERD_SIGNER_SESSION_STDIN=1",
		EnvSignerSock+"="+cfg.SocketPath,
		"HERD_ADMISSION_LEDGER="+ledPath,
		KeyDirEnv+"="+cfg.KeyDir,
	)
	cmd := runAs(topo.SignerUID, env, bin, "signer-boundary", "serve",
		"--key", keyPath, "--socket", cfg.SocketPath, "--session-key-stdin",
		"--admission-ledger", ledPath)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("%w: start serve as uid %d: %v", ErrProvisioning, topo.SignerUID, err)
	}
	go func() {
		h.waitErr <- cmd.Wait()
		close(h.waitErr)
	}()
	if _, err := stdin.Write([]byte(hexKey)); err != nil {
		_ = cmd.Process.Kill()
		return nil, err
	}
	_ = stdin.Close()
	// Wipe local session bytes after handoff to S stdin + sealed file.
	for i := range sk {
		sk[i] = 0
	}
	h.cmd = cmd
	h.SignerPID = cmd.Process.Pid

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-h.waitErr:
			return nil, fmt.Errorf("%w: serve exited before ready: %v", ErrProvisioning, err)
		default:
		}
		if fi, err := os.Lstat(cfg.SocketPath); err == nil && fi.Mode()&os.ModeSocket != 0 {
			goto socketReady
		}
		time.Sleep(40 * time.Millisecond)
	}
	_ = h.Close()
	return nil, fmt.Errorf("%w: serve did not create socket", ErrProvisioning)

socketReady:
	_ = atomicWriteFile(SignerPIDPath(cfg.KeyDir), []byte(itoa(h.SignerPID)+"\n"), 0o644)
	_ = os.Chown(SignerPIDPath(cfg.KeyDir), topo.RequesterUID, topo.SocketGID)

	if !cfg.SkipLiveProve {
		if err := LiveLaunchProve(h, runAs, bin); err != nil {
			_ = h.Close()
			return nil, fmt.Errorf("%w: live launch prove failed: %v", ErrProvisioning, err)
		}
	}
	// Detach: do not require caller to Wait; serve is supervised via pid file.
	return h, nil
}

// LiveLaunchProve runs exact probes as real B and real R via RunAs.
// Harness ambiguity is BLOCKED (fail closed).
func LiveLaunchProve(h *LaunchHandle, runAs func(int, []string, string, ...string) *exec.Cmd, herdBin string) error {
	if h == nil || h.SignerPID <= 0 {
		return fmt.Errorf("%w: no live serve process", ErrProvisioning)
	}
	if err := syscall.Kill(h.SignerPID, 0); err != nil {
		return fmt.Errorf("%w: serve not alive: %v", ErrProvisioning, err)
	}
	fi, err := os.Lstat(h.SocketPath)
	if err != nil {
		return err
	}
	if fi.Mode().Perm() == 0o600 {
		return fmt.Errorf("%w: socket is 0600 — R cannot connect", ErrProvisioning)
	}
	if runAs == nil {
		runAs = defaultRunAs
	}

	// Compile a small probe helper into temp (or use herd prove-helper).
	helper, err := buildLiveProbeHelper()
	if err != nil {
		return fmt.Errorf("%w: build live probe helper: %v", ErrProvisioning, err)
	}
	defer os.Remove(helper)

	// --- B: key-read must deny with EACCES/EPERM ---
	out, err := runCmdCapture(runAs(h.Topo.BuilderUID, probeEnv(h), helper, "keyread", h.KeyPath))
	if err == nil || strings.Contains(out, "KEY_READ_OK") {
		return fmt.Errorf("%w: builder key-read succeeded", ErrAdversarialSuccess)
	}
	if !strings.Contains(out, "KEY_READ_DENIED") {
		return fmt.Errorf("%w: builder key-read harness ambiguity: %s err=%v", ErrProvisioning, out, err)
	}

	// --- B: attach to exact S pid must EPERM/EACCES ---
	out, err = runCmdCapture(runAs(h.Topo.BuilderUID, probeEnv(h), helper, "attach", itoa(h.SignerPID)))
	if err == nil || strings.Contains(out, "ATTACH_OK") {
		return fmt.Errorf("%w: builder attach succeeded", ErrAdversarialSuccess)
	}
	if !strings.Contains(out, "ATTACH_DENIED") && !strings.Contains(out, "EPERM") &&
		!strings.Contains(out, "EACCES") && !strings.Contains(out, "operation not permitted") {
		return fmt.Errorf("%w: builder attach harness ambiguity: %s err=%v", ErrProvisioning, out, err)
	}
	if strings.Contains(out, "ATTACH_HARNESS") {
		return fmt.Errorf("%w: builder attach harness failure: %s", ErrProvisioning, out)
	}

	// --- B: sign-verdict with stolen sealed session must UNAUTHORIZED_PEER ---
	// B should not read sealed session; try with empty mac and with forced read fail.
	out, err = runCmdCapture(runAs(h.Topo.BuilderUID, probeEnv(h), helper, "sign", h.SocketPath, "badmac", "nonce-b"))
	if err == nil || strings.Contains(out, "SIG_OK") {
		return fmt.Errorf("%w: builder sign succeeded", ErrAdversarialSuccess)
	}
	// Accept UNAUTHORIZED_PEER or dial/MAC denial — not success.

	// --- R: admit + sign with sealed session ---
	sk, err := LoadSealedSession(h.KeyDir)
	if err != nil {
		// Load as R via RunAs helper
		out, err2 := runCmdCapture(runAs(h.Topo.RequesterUID, probeEnv(h), helper, "sign-admitted",
			h.SocketPath, h.KeyDir, h.LedgerPath))
		if err2 != nil || !strings.Contains(out, "SIG_OK") {
			return fmt.Errorf("%w: R sign failed: sealed=%v out=%s err=%v", ErrProvisioning, err, out, err2)
		}
	} else {
		// We can load sealed only if we are R; grant+sign in-process.
		if os.Getuid() == h.Topo.RequesterUID {
			led, err := OpenAdmissionLedgerTopo(h.LedgerPath, h.Topo)
			if err != nil {
				return err
			}
			rec := AdmissionRecord{
				TokenID:      "launch-prove-" + itoa(int(time.Now().UnixNano())),
				CandidateSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				BaseSHA:      "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
				PatchID:      "launch-prove",
				SessionID:    "session-launch-prove",
				Verdict:      "APPROVED",
				SingleUse:    true,
			}
			if err := led.AppendGrant(rec); err != nil {
				return err
			}
			req := NewVerdictRequest(rec.CandidateSHA, rec.BaseSHA, rec.PatchID, rec.Verdict, rec.SessionID, nil)
			req.Nonce = "launch-prove-nonce-1"
			sig, err := signRequestOverIPC(h.SocketPath, sk, &req)
			if err != nil || len(sig) == 0 {
				return fmt.Errorf("%w: R sign failed: %v", ErrProvisioning, err)
			}
		} else {
			out, err2 := runCmdCapture(runAs(h.Topo.RequesterUID, probeEnv(h), helper, "sign-admitted",
				h.SocketPath, h.KeyDir, h.LedgerPath))
			if err2 != nil || !strings.Contains(out, "SIG_OK") {
				return fmt.Errorf("%w: R RunAs sign failed: %s err=%v", ErrProvisioning, out, err2)
			}
		}
	}

	if err := syscall.Kill(h.SignerPID, 0); err != nil {
		return fmt.Errorf("%w: serve died during prove: %v", ErrProvisioning, err)
	}
	return nil
}

func probeEnv(h *LaunchHandle) []string {
	return []string{
		EnvSignerUID + "=" + itoa(h.Topo.SignerUID),
		EnvRequesterUID + "=" + itoa(h.Topo.RequesterUID),
		EnvBuilderUID + "=" + itoa(h.Topo.BuilderUID),
		EnvSocketGID + "=" + itoa(h.Topo.SocketGID),
		KeyDirEnv + "=" + h.KeyDir,
		EnvSignerSock + "=" + h.SocketPath,
		"HERD_ADMISSION_LEDGER=" + h.LedgerPath,
		"PATH=" + os.Getenv("PATH"),
		"HOME=/tmp",
	}
}

func runCmdCapture(cmd *exec.Cmd) (string, error) {
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func publishPublicFromKey(keyPath, repoRoot string, topo Topology, runAs func(int, []string, string, ...string) *exec.Cmd, herdBin string) error {
	// Prefer opening as current if we own the key (S or root before chown).
	if pub, err := publicFromSeedFile(keyPath, topo.SignerUID); err == nil {
		return forcePublishPublicKey(repoRoot, pub)
	}
	// Root may open by temporarily checking — openKeyVerified requires wantUID.
	// Try raw read as root (uid 0).
	if os.Getuid() == 0 {
		data, err := os.ReadFile(keyPath)
		if err != nil {
			return err
		}
		seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
		for i := range data {
			data[i] = 0
		}
		if err != nil || len(seed) != ed25519.SeedSize {
			return fmt.Errorf("corrupt seed for publish")
		}
		priv := ed25519.NewKeyFromSeed(seed)
		for i := range seed {
			seed[i] = 0
		}
		pub := priv.Public().(ed25519.PublicKey)
		for i := range priv {
			priv[i] = 0
		}
		return forcePublishPublicKey(repoRoot, pub)
	}
	return fmt.Errorf("%w: cannot publish public key without seed access as S or root", ErrProvisioning)
}

func generateInitialKey(path string, topo Topology) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	seed := priv.Seed()
	err = atomicWriteFile(path, []byte(hex.EncodeToString(seed)+"\n"), 0o600)
	for i := range seed {
		seed[i] = 0
	}
	for i := range priv {
		priv[i] = 0
	}
	if err != nil {
		return err
	}
	_ = os.Chown(path, topo.SignerUID, topo.SocketGID)
	_ = os.Chmod(filepath.Dir(path), 0o700)
	_ = os.Chown(filepath.Dir(path), topo.SignerUID, topo.SocketGID)
	return nil
}

func publicFromSeedFile(path string, wantUID int) (ed25519.PublicKey, error) {
	f, err := openKeyVerified(path, wantUID)
	if err != nil {
		return nil, err
	}
	data, err := ioReadAllClose(f)
	if err != nil {
		return nil, err
	}
	seed, err := hex.DecodeString(strings.TrimSpace(string(data)))
	for i := range data {
		data[i] = 0
	}
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("corrupt seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	for i := range seed {
		seed[i] = 0
	}
	pub := priv.Public().(ed25519.PublicKey)
	for i := range priv {
		priv[i] = 0
	}
	return pub, nil
}

func defaultRunAs(uid int, env []string, name string, args ...string) *exec.Cmd {
	if uid == os.Getuid() {
		cmd := exec.Command(name, args...)
		cmd.Env = env
		return cmd
	}
	// Prefer setpriv when available (containers), else sudo -n.
	if _, err := exec.LookPath("setpriv"); err == nil {
		full := append([]string{"--reuid=" + itoa(uid), "--init-groups", "--", name}, args...)
		cmd := exec.Command("setpriv", full...)
		cmd.Env = env
		return cmd
	}
	full := append([]string{"-n", "-u", "#" + strconv.Itoa(uid), "--", name}, args...)
	cmd := exec.Command("sudo", full...)
	cmd.Env = env
	return cmd
}

// RunAsUID spawns helpers as a given uid.
func RunAsUID(uid int, env []string, name string, args ...string) *exec.Cmd {
	return defaultRunAs(uid, env, name, args...)
}

// LiveTopologyProvisioned reports whether topology env is loadable.
func LiveTopologyProvisioned() bool {
	_, err := LoadTopology()
	return err == nil
}

func filterEnv(in []string, dropPrefix string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		if strings.HasPrefix(e, dropPrefix+"=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// SignerPIDPath returns attest/signer.pid
func SignerPIDPath(keyDir string) string {
	return filepath.Join(keyDir, AttestSubdir, "signer.pid")
}

// ReadSignerPID loads the last launched serve pid.
func ReadSignerPID(keyDir string) (int, error) {
	data, err := os.ReadFile(SignerPIDPath(keyDir))
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

// EnforceBuilderIsolation is the production hook (test-stubbable like EnforceAtLaunch).
var EnforceBuilderIsolation = RequireBuilderIsolation

// RequireBuilderIsolation fails closed when workers would share R's UID.
func RequireBuilderIsolation() error {
	topo, err := LoadTopology()
	if err != nil {
		return err
	}
	// Coordinator launching workers must be R; workers must be B ≠ R.
	if os.Getuid() != topo.RequesterUID {
		return fmt.Errorf("%w: dispatch/coordinator must run as HERD_REQUESTER_UID %d (got %d)",
			ErrProvisioning, topo.RequesterUID, os.Getuid())
	}
	if topo.BuilderUID == topo.RequesterUID {
		return fmt.Errorf("%w: BuilderUID equals RequesterUID", ErrUnsupportedPlatform)
	}
	return nil
}

// BuilderUID returns the kernel uid workers must execute as.
func BuilderUID() (int, error) {
	topo, err := LoadTopology()
	if err != nil {
		return 0, err
	}
	return topo.BuilderUID, nil
}
