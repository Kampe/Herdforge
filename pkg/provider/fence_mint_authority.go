package provider

import (
	cryptorand "crypto/rand"
	"path/filepath"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// Production coordinator mint authority (FAC-564, the capability the code
// previously deferred to "FAC-169" with no card tracking it).
//
// The two refusals that were already here are correct and stay refused:
//
//   - env mint is forgeable: any process can set a variable.
//   - a mode-0600 file in a shared claim dir is readable by any same-UID worker,
//     so filesystem permission is not an authority boundary when the coordinator
//     and its workers run as the same user.
//
// Under one UID there are exactly two boundaries the kernel will actually
// enforce for us, and this file provides both:
//
//   - ADDRESS SPACE. If the coordinator runs the broker in its own process, the
//     mint secret never leaves that process — no file, no environment, no
//     socket, nothing on disk for a worker to read. This is the strongest
//     option and the one to prefer.
//   - FD TABLE. If the broker must be a separate process, the secret is handed
//     over an inherited pipe. A worker started independently has no such entry
//     in its file-descriptor table, and a descriptor number is not a secret:
//     without inheritance the read fails.
//
// Neither mechanism puts the secret anywhere a same-UID worker can read it.

// envFenceMintFD names the inherited descriptor carrying the mint secret. The
// VALUE is a descriptor number, not a credential: it is useless to a process
// that did not inherit the descriptor, which is exactly why this is safe to
// pass in the environment while the secret itself never is.
const envFenceMintFD = "HERD_FENCE_MINT_FD"

// CoordinatorMinterInProcess grants mint authority to the process that OWNS the
// broker.
//
// The boundary is the address space: b.mintToken was generated in this process
// and is never written to a file, an environment variable, or the wire, so no
// same-UID worker can read it. Prefer this whenever the coordinator can host
// the broker itself.
func CoordinatorMinterInProcess(b *FenceBroker) (*FenceBrokerMinter, error) {
	if b == nil {
		return nil, fmt.Errorf("provider: in-process mint requires a live broker owned by this process")
	}
	if strings.TrimSpace(b.mintToken) == "" {
		return nil, fmt.Errorf("provider: broker has no mint token")
	}
	m := &FenceBrokerMinter{mintSecret: b.mintToken, claimDir: b.claimDir}
	if err := bindToBrokerEndpoint(b, func(sock string) { m.unixSocket, m.baseURL = sock, unixBaseURL }, m.bindURL); err != nil {
		return nil, err
	}
	return m, nil
}

// GrantMintToChild hands this broker's mint authority to one child process over
// an inherited pipe, and returns a closer the parent calls after Start.
//
// The parent writes the secret into a pipe whose read end the child inherits.
// The secret is never placed in the child's environment or on disk. Only this
// exact child can read it, because only this child has the descriptor.
func (b *FenceBroker) GrantMintToChild(cmd *exec.Cmd) (func(), error) {
	if b == nil || strings.TrimSpace(b.mintToken) == "" {
		return nil, fmt.Errorf("provider: broker has no mint token to grant")
	}
	if cmd == nil {
		return nil, fmt.Errorf("provider: nil child command")
	}
	if cmd.Process != nil {
		return nil, fmt.Errorf("provider: cannot grant mint to an already-started child")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("provider: mint pipe: %w", err)
	}
	// ExtraFiles[0] becomes descriptor 3 in the child.
	cmd.ExtraFiles = append(cmd.ExtraFiles, r)
	childFD := 2 + len(cmd.ExtraFiles)
	cmd.Env = append(cmd.Environ(), fmt.Sprintf("%s=%d", envFenceMintFD, childFD))

	secret := b.mintToken
	deliver := func() {
		// Write and close in the background: the child may not read until it
		// needs to mint, and a small secret fits the pipe buffer regardless.
		go func() {
			defer func() { _ = w.Close() }()
			_, _ = w.Write([]byte(secret + "\n"))
		}()
		// The parent must not retain the read end, or the child sees no EOF.
		_ = r.Close()
	}
	return deliver, nil
}

// NewFenceBrokerMinterFromInheritedFD constructs a minter from a descriptor
// inherited from the broker process.
//
// The descriptor must be a PIPE. A worker cannot satisfy this by writing its own
// secret to a file and pointing the variable at it, and cannot satisfy it at all
// without an inherited descriptor: it simply has nothing at that number.
//
// The descriptor is consumed exactly once and closed, so the secret does not
// linger in the descriptor table for a later exec to inherit.
func NewFenceBrokerMinterFromInheritedFD(brokerURL string) (*FenceBrokerMinter, error) {
	raw := strings.TrimSpace(os.Getenv(envFenceMintFD))
	if raw == "" {
		return nil, fmt.Errorf("provider: no inherited mint descriptor (%s unset); a worker cannot mint", envFenceMintFD)
	}
	fd, err := strconv.Atoi(raw)
	if err != nil || fd < 3 {
		return nil, fmt.Errorf("provider: %s must be an inherited descriptor number >= 3, got %q", envFenceMintFD, raw)
	}
	// Refuse anything that is not a pipe. A regular file would let a worker
	// forge authority with material it wrote itself, which is the same defect
	// as the claim-dir credential this replaces.
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("provider: mint descriptor %d is not open (not inherited): %w", fd, err)
	}
	if st.Mode&unix.S_IFMT != unix.S_IFIFO {
		return nil, fmt.Errorf("provider: mint descriptor %d must be a pipe, not a file or socket (forgeable)", fd)
	}
	f := os.NewFile(uintptr(fd), "herd-fence-mint")
	if f == nil {
		return nil, fmt.Errorf("provider: mint descriptor %d unusable", fd)
	}
	defer func() {
		_ = f.Close()
		// Consume once: clear the variable so a later construction in this
		// process fails loudly rather than reading a closed descriptor.
		_ = os.Unsetenv(envFenceMintFD)
	}()
	buf := make([]byte, 4096)
	n, rerr := f.Read(buf)
	if n == 0 {
		if rerr != nil {
			return nil, fmt.Errorf("provider: read mint descriptor: %w", rerr)
		}
		return nil, fmt.Errorf("provider: mint descriptor delivered no secret")
	}
	secret := strings.TrimSpace(string(buf[:n]))
	if len(secret) < 16 {
		return nil, fmt.Errorf("provider: inherited mint secret too short")
	}
	// The secret must never ALSO be in the environment: that would mean a
	// launcher leaked it to every child, defeating the descriptor boundary.
	if err := refuseEnvMintLeak(); err != nil {
		return nil, err
	}
	brokerURL, err = resolveMinterBrokerURL(brokerURL)
	if err != nil {
		return nil, err
	}
	m := &FenceBrokerMinter{mintSecret: secret}
	if err := m.bindURL(brokerURL); err != nil {
		return nil, err
	}
	return m, nil
}

// CoordinatorBrokerOptions configures a coordinator that hosts its own broker.
type CoordinatorBrokerOptions struct {
	ClaimDir string
	// ListenAddr defaults to "unix": a socket under the claim dir, gated by
	// filesystem permissions as well as the token. Set "127.0.0.1:0" when the
	// claim dir path is too long for a unix socket on this platform.
	ListenAddr      string
	UpstreamURL     string
	UpstreamProject string
	UpstreamCLI     bool
}

// CoordinatorBroker is a broker owned by this process plus the authority to mint
// against it.
type CoordinatorBroker struct {
	Broker  *FenceBroker
	Minter  *FenceBrokerMinter
	Client  *FenceBrokerClient
	closeFn func() error
}

// Close releases the broker and its claim-dir lock.
func (c *CoordinatorBroker) Close() error {
	if c == nil || c.closeFn == nil {
		return nil
	}
	return c.closeFn()
}

// StartCoordinatorBroker starts a fence broker INSIDE the coordinator process
// and returns mint authority over it.
//
// This is the production path FAC-564 was missing. Both credentials are
// generated here and never leave the process: the worker token is used by this
// process's own client, and the mint token backs the in-process minter. Nothing
// is written to the claim dir and nothing is placed in the environment, so no
// same-UID worker can read either one.
//
// The claim-dir flock still guarantees one live broker, so this fails closed if
// a standalone broker is already serving that claim volume. When a coordinator
// hosts its own broker, do not also run a standalone one.
func StartCoordinatorBroker(opts CoordinatorBrokerOptions) (*CoordinatorBroker, error) {
	if strings.TrimSpace(opts.ClaimDir) == "" {
		return nil, fmt.Errorf("provider: coordinator broker requires a claim dir")
	}
	workerTok, err := randomCredential()
	if err != nil {
		return nil, err
	}
	mintTok, err := randomCredential()
	if err != nil {
		return nil, err
	}
	if workerTok == mintTok {
		return nil, fmt.Errorf("provider: generated identical credentials")
	}
	listen := strings.TrimSpace(opts.ListenAddr)
	if listen == "" {
		listen = "unix"
	}
	if listen == "unix" {
		// A unix socket path has a hard platform limit (104 bytes on Darwin,
		// 108 on Linux) and bind fails with a bare "invalid argument" that says
		// nothing about length. Refuse with the real reason instead.
		if sock, serr := filepath.Abs(filepath.Join(opts.ClaimDir, brokerSockLeaf)); serr == nil && len(sock) > 100 {
			return nil, fmt.Errorf("provider: claim-dir socket path is %d bytes, over the platform limit for a unix socket: use a shorter HERD_CLAIM_DIR, or set ListenAddr to 127.0.0.1:0", len(sock))
		}
	}
	b, err := StartFenceBroker(FenceBrokerConfig{
		ClaimDir:        opts.ClaimDir,
		ListenAddr:      listen,
		Token:           workerTok,
		MintToken:       mintTok,
		UpstreamURL:     opts.UpstreamURL,
		UpstreamProject: opts.UpstreamProject,
		UpstreamCLI:     opts.UpstreamCLI,
	})
	if err != nil {
		return nil, fmt.Errorf("provider: start coordinator-owned broker: %w", err)
	}
	m, err := CoordinatorMinterInProcess(b)
	if err != nil {
		_ = b.Close()
		return nil, err
	}
	c := &FenceBrokerClient{Token: workerTok}
	if err := bindToBrokerEndpoint(b, func(sock string) { c.UnixSocket, c.BaseURL = sock, unixBaseURL }, c.bindURL); err != nil {
		_ = b.Close()
		return nil, err
	}
	return &CoordinatorBroker{Broker: b, Minter: m, Client: c, closeFn: b.Close}, nil
}

// randomCredential returns a fresh high-entropy credential. It is generated in
// memory and never persisted: persistence is what made the previous mechanism
// forgeable by a same-UID worker.
func randomCredential() (string, error) {
	buf := make([]byte, 32)
	if _, err := cryptorand.Read(buf); err != nil {
		return "", fmt.Errorf("provider: generate credential: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// coordinatorOwnsBroker reports whether this process was explicitly asked to
// host the fence broker.
//
// Deliberately opt-in: hosting takes the claim-dir flock, so a process that did
// this by default would lock out a standalone broker and every other
// coordinator on the volume. It is also refused for anything that looks like a
// worker, because a worker that hosted a broker would hold mint authority — the
// exact thing that must remain impossible.
func coordinatorOwnsBroker() bool {
	if strings.TrimSpace(os.Getenv(envFenceCoordinator)) != "1" {
		return false
	}
	if role := strings.ToLower(strings.TrimSpace(os.Getenv("HERD_ROLE"))); role == "worker" || role == "builder" || role == "reviewer" {
		return false
	}
	return true
}

// bindToBrokerEndpoint applies one rule for reaching a live broker: prefer its
// unix socket, otherwise its loopback URL, and refuse when it is not listening.
//
// The minter and the client used to each carry their own copy of this, which is
// how one could end up bound to a socket while the other silently held an empty
// base URL.
func bindToBrokerEndpoint(b *FenceBroker, useSocket func(string), useURL func(string) error) error {
	if b == nil {
		return fmt.Errorf("provider: no broker to bind")
	}
	if sock := b.UnixSocket(); strings.TrimSpace(sock) != "" {
		useSocket(sock)
		return nil
	}
	url := b.ClientBaseURL()
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("provider: broker is not listening; cannot bind")
	}
	return useURL(url)
}
