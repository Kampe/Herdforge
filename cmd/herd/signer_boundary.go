package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/signerboundary"
)

// Production consumer: kernel three-UID topology + SocketGID ACL.
// Peer auth is SO_PEERCRED UID only — not exe path / HERD_ROLE / env.
func runSignerBoundary() {
	if len(os.Args) < 3 {
		printSignerBoundaryUsage()
		os.Exit(1)
	}
	switch os.Args[2] {
	case "serve":
		runSignerBoundaryServe(os.Args[3:])
	case "launch":
		runSignerBoundaryLaunch(os.Args[3:])
	case "establish":
		runSignerBoundaryEstablish(os.Args[3:])
	case "status":
		runSignerBoundaryStatus(os.Args[3:])
	case "prove":
		runSignerBoundaryProve(os.Args[3:])
	case "admit":
		runSignerBoundaryAdmit(os.Args[3:])
	case "sign-verdict":
		runSignerBoundarySignVerdict(os.Args[3:])
	case "rotate-key":
		runSignerBoundaryRotateKey(os.Args[3:])
	case "revoke":
		runSignerBoundaryRevoke(os.Args[3:])
	case "sign", "sign-bytes":
		fmt.Fprintln(os.Stderr, "signer-boundary: no general sign oracle. Use sign-verdict.")
		os.Exit(1)
	case "--help", "-h", "help":
		printSignerBoundaryUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q\n", os.Args[2])
		printSignerBoundaryUsage()
		os.Exit(1)
	}
}

func printSignerBoundaryUsage() {
	fmt.Fprintln(os.Stderr, `Usage:
  # Topology (required): HERD_SIGNER_UID / HERD_REQUESTER_UID / HERD_BUILDER_UID / HERD_SIGNER_SOCK_GID
  herd signer-boundary launch --key-dir DIR --socket PATH --repo PATH --identity NAME
  herd signer-boundary serve --key PATH --socket PATH --admission-ledger PATH \
      (--session-key-fd N | --session-key-stdin)
  herd signer-boundary establish|status|prove
  herd signer-boundary admit --candidate --base --patch --verdict --session [--key-dir]
  herd signer-boundary sign-verdict --candidate --base --patch --verdict --session --payload-hex
  herd signer-boundary rotate-key --key-dir DIR --identity NAME --socket PATH [--repo] [--restart]
  herd signer-boundary revoke --key-dir DIR --identity NAME --socket PATH [--wipe-key]

Session keys: FD/stdin only — never print secret hex on stdout.
Admission: durable ledger file (FAC-145 channel across S process boundary).
Missing topology is typed BLOCKED. RequireLaunchReady is fail-closed.`)
}

func runSignerBoundaryServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyPath := fs.String("key", "", "")
	sock := fs.String("socket", "", "")
	keyFD := fs.Int("session-key-fd", -1, "")
	keyStdin := fs.Bool("session-key-stdin", false, "")
	ledger := fs.String("admission-ledger", "", "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *keyPath == "" || *sock == "" {
		fmt.Fprintln(os.Stderr, "serve: --key --socket required")
		os.Exit(1)
	}
	topo, err := signerboundary.LoadTopology()
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: topology: %v\n", err)
		os.Exit(1)
	}
	if os.Getuid() != topo.SignerUID {
		fmt.Fprintf(os.Stderr, "serve: must run as HERD_SIGNER_UID=%d (got %d)\n", topo.SignerUID, os.Getuid())
		os.Exit(1)
	}
	sk, err := signerboundary.LoadServeSessionKey(*keyFD, *keyStdin, "")
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: session key: %v\n", err)
		os.Exit(1)
	}
	led := strings.TrimSpace(*ledger)
	if led == "" {
		led = strings.TrimSpace(os.Getenv("HERD_ADMISSION_LEDGER"))
	}
	if led == "" {
		fmt.Fprintln(os.Stderr, "serve: --admission-ledger (or HERD_ADMISSION_LEDGER) required — durable FAC-145 channel")
		os.Exit(1)
	}
	srv, err := signerboundary.StartServer(signerboundary.ServeOptions{
		KeyPath: *keyPath, SocketPath: *sock, SessionKey: sk, Topology: topo,
		AdmissionLedgerPath: led, RequireDurableAdmission: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "signer-boundary serve pid=%d socket_acl=0660 gid=%d ledger=%s\n",
		srv.PID(), topo.SocketGID, filepath.Base(led))
	fmt.Printf("HERD_SIGNER_PID=%d\n", srv.PID())
	if err := srv.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}

func runSignerBoundaryLaunch(args []string) {
	fs := flag.NewFlagSet("launch", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyDir := fs.String("key-dir", "", "")
	sock := fs.String("socket", "", "")
	repo := fs.String("repo", ".", "")
	identity := fs.String("identity", "", "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *keyDir == "" || *sock == "" {
		fmt.Fprintln(os.Stderr, "launch: --key-dir and --socket required")
		os.Exit(1)
	}
	id := *identity
	if id == "" {
		id = defaultSignerIdentity(*repo)
	}
	self, err := os.Executable()
	if err != nil {
		self = "herd"
	}
	// Detached serve + sealed session.rkey for R; live RunAs B/R prove required.
	h, err := signerboundary.ProvisionAndLaunch(signerboundary.LaunchConfig{
		KeyDir: *keyDir, RepoRoot: *repo, Identity: id, SocketPath: *sock, HerdBinary: self,
		DetachServe: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "launch: %v\n", err)
		os.Exit(1)
	}
	// Metadata only — no secret hex, no transient FD numbers.
	fmt.Printf("HERD_SIGNER_PID=%d\n", h.SignerPID)
	fmt.Printf("HERD_SIGNER_SOCK=%s\n", h.SocketPath)
	fmt.Printf("HERD_ADMISSION_LEDGER=%s\n", h.LedgerPath)
	fmt.Printf("HERD_KEY_DIR=%s\n", h.KeyDir)
	fmt.Printf("HERD_SEALED_SESSION=%s\n", h.SealedSession)
	fmt.Fprintln(os.Stderr, "launch: live prove passed; serve detached; R uses sealed session.rkey (0600 R-owned)")
	// Do not block on serve Wait — supervisor reattaches via HERD_SIGNER_PID.
}

func signerBoundaryOptsFromFlags(args []string) signerboundary.Options {
	fs := flag.NewFlagSet("signer-boundary", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	repo := fs.String("repo", ".", "")
	keyDir := fs.String("key-dir", "", "")
	identity := fs.String("identity", "", "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	opts := signerboundary.Options{RepoRoot: *repo, RequireSeparateUID: true}
	if *keyDir != "" {
		opts.KeyDir = *keyDir
	} else {
		dir, err := signerboundary.ResolveKeyDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		opts.KeyDir = dir
	}
	if *identity != "" {
		opts.Identity = *identity
	} else {
		opts.Identity = defaultSignerIdentity(opts.RepoRoot)
	}
	if abs, err := filepath.Abs(opts.RepoRoot); err == nil {
		opts.RepoRoot = abs
	}
	return opts
}

func defaultSignerIdentity(repoRoot string) string {
	cfgPath := filepath.Join(repoRoot, ".herd", "herd.yaml")
	if cfg, err := config.LoadConfig(cfgPath); err == nil && cfg != nil && strings.TrimSpace(cfg.Project.Name) != "" {
		return strings.TrimSpace(cfg.Project.Name)
	}
	base := filepath.Base(repoRoot)
	if base == "" || base == "." {
		return "herdforge"
	}
	return base
}

func runSignerBoundaryEstablish(args []string) {
	opts := signerBoundaryOptsFromFlags(args)
	b, err := signerboundary.Provision(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "establish: %v\n", err)
		os.Exit(1)
	}
	att := b.Attestation()
	fmt.Printf("established mechanism=%s probe=%s signer_uid=%d\n", att.Mechanism, att.ProbeDigest, att.KeyOwnerUID)
}

func runSignerBoundaryStatus(args []string) {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyDir := fs.String("key-dir", "", "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	dir := *keyDir
	if dir == "" {
		var err error
		dir, err = signerboundary.ResolveKeyDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	att, err := signerboundary.RequireReady(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "NOT READY: signer boundary not established: %v\n", err)
		os.Exit(1)
	}
	out := att
	if out.SocketPath != "" {
		out.SocketPath = filepath.Base(out.SocketPath)
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

func runSignerBoundaryProve(args []string) {
	opts := signerBoundaryOptsFromFlags(args)
	b, err := signerboundary.Reprove(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "prove: %v\n", err)
		os.Exit(1)
	}
	if err := b.AdversarialProbe(); err != nil {
		fmt.Fprintf(os.Stderr, "prove: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("proved mechanism=%s\n", b.Attestation().Mechanism)
}

func runSignerBoundarySignVerdict(args []string) {
	fs := flag.NewFlagSet("sign-verdict", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cand := fs.String("candidate", "", "")
	base := fs.String("base", "", "")
	patch := fs.String("patch", "", "")
	verdict := fs.String("verdict", "", "")
	session := fs.String("session", "", "")
	payloadHex := fs.String("payload-hex", "", "")
	keyDir := fs.String("key-dir", "", "")
	repo := fs.String("repo", ".", "")
	identity := fs.String("identity", "", "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *cand == "" || *base == "" || *patch == "" || *verdict == "" || *session == "" || *payloadHex == "" {
		fmt.Fprintln(os.Stderr, "sign-verdict: all of --candidate --base --patch --verdict --session --payload-hex required")
		os.Exit(1)
	}
	payload, err := hex.DecodeString(*payloadHex)
	if err != nil {
		fmt.Fprintf(os.Stderr, "payload-hex: %v\n", err)
		os.Exit(1)
	}
	opts := signerboundary.Options{RepoRoot: *repo, RequireSeparateUID: true}
	if *keyDir != "" {
		opts.KeyDir = *keyDir
	} else {
		opts.KeyDir, err = signerboundary.ResolveKeyDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	if *identity != "" {
		opts.Identity = *identity
	} else {
		opts.Identity = defaultSignerIdentity(opts.RepoRoot)
	}
	b, err := signerboundary.Open(opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	sig, err := b.SignVerdict(*cand, *base, *patch, *verdict, *session, payload)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	fmt.Println(hex.EncodeToString(sig))
}

func runSignerBoundaryAdmit(args []string) {
	fs := flag.NewFlagSet("admit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cand := fs.String("candidate", "", "")
	base := fs.String("base", "", "")
	patch := fs.String("patch", "", "")
	verdict := fs.String("verdict", "", "")
	session := fs.String("session", "", "")
	keyDir := fs.String("key-dir", "", "")
	ttl := fs.Int64("ttl", 0, "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *cand == "" || *base == "" || *patch == "" || *verdict == "" || *session == "" {
		fmt.Fprintln(os.Stderr, "admit: --candidate --base --patch --verdict --session required (as R)")
		os.Exit(1)
	}
	dir := *keyDir
	if dir == "" {
		var err error
		dir, err = signerboundary.ResolveKeyDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
	}
	if err := signerboundary.MustBeRequester(); err != nil {
		fmt.Fprintf(os.Stderr, "admit: %v\n", err)
		os.Exit(1)
	}
	led, err := signerboundary.OpenAdmissionLedger(signerboundary.AdmissionLedgerPath(dir))
	if err != nil {
		fmt.Fprintf(os.Stderr, "admit: %v\n", err)
		os.Exit(1)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		fmt.Fprintf(os.Stderr, "admit: %v\n", err)
		os.Exit(1)
	}
	token := hex.EncodeToString(raw)
	rec := signerboundary.AdmissionRecord{
		TokenID: token, CandidateSHA: *cand, BaseSHA: *base, PatchID: *patch,
		SessionID: *session, Verdict: *verdict, SingleUse: true,
	}
	if *ttl > 0 {
		rec.ExpiresUnix = time.Now().Unix() + *ttl
		rec.SingleUse = false
	}
	if err := led.AppendGrant(rec); err != nil {
		fmt.Fprintf(os.Stderr, "admit: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("admitted token=%s\n", token)
}

func runSignerBoundaryRotateKey(args []string) {
	fs := flag.NewFlagSet("rotate-key", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyDir := fs.String("key-dir", "", "")
	identity := fs.String("identity", "", "")
	repo := fs.String("repo", "", "")
	sock := fs.String("socket", "", "")
	restart := fs.Bool("restart", false, "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *keyDir == "" || *identity == "" {
		fmt.Fprintln(os.Stderr, "rotate-key: --key-dir and --identity required (run as HERD_SIGNER_UID)")
		os.Exit(1)
	}
	topo, err := signerboundary.LoadTopology()
	if err != nil {
		fmt.Fprintf(os.Stderr, "rotate-key: %v\n", err)
		os.Exit(1)
	}
	self, _ := os.Executable()
	var sk signerboundary.SessionKey
	if *restart {
		// Session from sealed R path — but rotate runs as S; load via file owned by R
		// is not readable by S. Re-generate session for new serve after rotate.
		var err error
		sk, err = signerboundary.NewSessionKey()
		if err != nil {
			fmt.Fprintf(os.Stderr, "rotate-key: session: %v\n", err)
			os.Exit(1)
		}
	}
	// Seal for R BEFORE restarting serve. WriteSealedSession chowns to R, which
	// EPERMs when rotate runs as S (non-root); doing it after the restart left
	// the signer live under a session key no requester could ever read.
	if *restart && len(sk) > 0 {
		if err := signerboundary.WriteSealedSession(*keyDir, topo, sk); err != nil {
			fmt.Fprintf(os.Stderr, "rotate-key: seal session for R (before restart): %v\n", err)
			os.Exit(1)
		}
	}
	res, err := signerboundary.RotateKeyFull(signerboundary.RotateOptions{
		KeyDir: *keyDir, Identity: *identity, RepoRoot: *repo, Topology: topo,
		Publish: *repo != "", SocketPath: *sock, RestartServe: *restart, HerdBinary: self,
		AdmissionLedger: signerboundary.AdmissionLedgerPath(*keyDir),
		SessionKey:      sk,
	})
	if err != nil {
		// The sealed session names a serve that was never started.
		if *restart && len(sk) > 0 {
			_ = os.Remove(signerboundary.SealedSessionPath(*keyDir))
		}
		fmt.Fprintf(os.Stderr, "rotate-key: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("rotated key=%s pub=%s restarted=%v new_pid=%d\n",
		res.KeyPath, res.PublicHex, res.Restarted, res.NewSignerPID)
}

func runSignerBoundaryRevoke(args []string) {
	fs := flag.NewFlagSet("revoke", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	keyDir := fs.String("key-dir", "", "")
	identity := fs.String("identity", "", "")
	sock := fs.String("socket", "", "")
	wipeKey := fs.Bool("wipe-key", false, "")
	if err := fs.Parse(args); err != nil {
		os.Exit(1)
	}
	if *keyDir == "" {
		fmt.Fprintln(os.Stderr, "revoke: --key-dir required")
		os.Exit(1)
	}
	if *sock == "" {
		*sock = os.Getenv("HERD_SIGNER_SOCK")
	}
	if err := signerboundary.RevokeAuthority(*keyDir, *identity, *sock, *wipeKey); err != nil {
		fmt.Fprintf(os.Stderr, "revoke: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("revoked")
}
