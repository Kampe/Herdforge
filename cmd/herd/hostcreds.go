package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/security"
)

// runHostCreds is the FAC-170 compiled production caller.
// Authority is handle-backed (HERD_HOSTCREDS_HANDLES); raw API key env is not production.
func runHostCreds() {
	if len(os.Args) < 3 {
		printHostCredsUsage()
		os.Exit(2)
	}
	switch os.Args[2] {
	case "diagnose":
		os.Exit(runHostCredsDiagnose(os.Args[3:]))
	case "session":
		os.Exit(runHostCredsSession(os.Args[3:]))
	case "selftest":
		os.Exit(runHostCredsSelftest(os.Args[3:]))
	case "live":
		os.Exit(runHostCredsLive(os.Args[3:]))
	case "boundary":
		os.Exit(runHostCredsBoundary(os.Args[3:]))
	case "worker-probe":
		os.Exit(runHostCredsWorkerProbe(os.Args[3:]))
	case "-h", "--help", "help":
		printHostCredsUsage()
		os.Exit(0)
	default:
		fmt.Fprintf(os.Stderr, "hostcreds: unknown subcommand %q\n", os.Args[2])
		printHostCredsUsage()
		os.Exit(2)
	}
}

func printHostCredsUsage() {
	fmt.Fprintln(os.Stderr, `herd hostcreds — HostCreds (FAC-170)

Usage:
  herd hostcreds diagnose  --kind <grok|claude|codex>
  herd hostcreds session   --kind <grok|claude|codex>
  herd hostcreds selftest
  herd hostcreds boundary          # reports FAC-169 dependency status
  herd hostcreds live --kind <grok|claude|codex>
  herd hostcreds worker-probe --proxy URL --allow-host H --deny-host D --session S --nonce N --out FILE

Production secrets: HERD_HOSTCREDS_HANDLES (or FAC-169 IPC after merge)
OS isolation: FAC-169 (hard blocker). Live waits for FAC-169 + RequireOSBoundary.

Exit: 0 ok, 1 fatal, 2 BLOCKED/usage. Never prints credential bytes. No OpenCode.`)
}

func runHostCredsWorkerProbe(args []string) int {
	fs := flag.NewFlagSet("hostcreds worker-probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	proxy := fs.String("proxy", "", "HTTP proxy URL")
	allow := fs.String("allow-host", "", "allowlisted host for CONNECT")
	deny := fs.String("deny-host", "evil.example.invalid", "forbidden host")
	session := fs.String("session", "", "session id")
	nonce := fs.String("nonce", "", "capability nonce")
	out := fs.String("out", "", "result JSON path")
	claim := fs.String("claim", "", "exclusive client-port claim file path")
	connectOnly := fs.Bool("connect-only", false, "CONNECT status only (no full TLS request)")
	method := fs.String("method", "POST", "TLS HTTP method after CONNECT")
	path := fs.String("path", "/v1/chat/completions", "TLS HTTP path after CONNECT")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *proxy == "" || *allow == "" || *out == "" {
		fmt.Fprintln(os.Stderr, "worker-probe: --proxy --allow-host --out required")
		return 2
	}
	if err := security.RunWorkerProbeConfig(security.WorkerProbeConfig{
		ProxyURL:    *proxy,
		AllowHost:   *allow,
		DenyHost:    *deny,
		SessionID:   *session,
		Nonce:       *nonce,
		OutPath:     *out,
		ClaimPath:   *claim,
		ConnectOnly: *connectOnly,
		Method:      *method,
		Path:        *path,
	}); err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 1
	}
	return 0
}

func runHostCredsBoundary(args []string) int {
	fs := flag.NewFlagSet("hostcreds boundary", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	b, err := security.RequireOSBoundary()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		fmt.Fprintln(os.Stderr, "hostcreds: OS boundary is owned by FAC-169; do not invent a FAC-170 duplicate")
		return 2
	}
	fmt.Printf("HOSTCREDS_BOUNDARY ok mechanism=%s digest=%s\n", b.Mechanism(), b.ProbeDigest())
	return 0
}

func runHostCredsLive(args []string) int {
	fs := flag.NewFlagSet("hostcreds live", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kind := fs.String("kind", "", "grok|claude|codex")
	marker := fs.String("marker", "", "expected marker (must NOT appear in prompt; default random)")
	prompt := fs.String("prompt", "", "non-interactive prompt (must not contain marker)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(os.Stderr, "hostcreds live: --kind required")
		return 2
	}
	// Live refuses in-process HandleAuthority; requires FAC-169 IPC authority + boundary.
	sess, _, proof, err := security.StartAuthorLive(security.LiveConfig{
		Kind:          *kind,
		Prompt:        *prompt,
		AllowedMarker: *marker,
		// Authority must come from FAC-169 IPC after merge — nil triggers fac169/authority gates.
		Authority: nil,
	})
	if sess != nil {
		defer sess.Close()
	}
	if proof != nil {
		fmt.Printf("HOSTCREDS_LIVE session_id=%s kind=%s author_pid=%d prompt_in_argv=%v marker_reached=%v forbidden_denied=%v no_api_keys=%v boundary=%s\n",
			proof.SessionID, proof.Kind, proof.AuthorPID, proof.PromptInArgv, proof.ModelMarkerReached, proof.ForbiddenDenied, proof.NoAPIKeysInEnv, proof.BoundaryDigest)
		if proof.OutputSnippet != "" {
			fmt.Printf("output_snippet=%s\n", proof.OutputSnippet)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		return 2
	}
	if proof == nil || !proof.PromptInArgv || !proof.ForbiddenDenied || !proof.NoAPIKeysInEnv || !proof.ModelMarkerReached || !proof.BrokerReached {
		fmt.Fprintln(os.Stderr, "hostcreds live: incomplete exact-session proof")
		return 2
	}
	fmt.Println("hostcreds live: PASS (exact-session process proof)")
	return 0
}

func runHostCredsDiagnose(args []string) int {
	fs := flag.NewFlagSet("hostcreds diagnose", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kind := fs.String("kind", "", "author kind: grok, claude, or codex")
	asJSON := fs.Bool("json", false, "emit KindAuthDiagnosis JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(os.Stderr, "hostcreds diagnose: --kind required")
		return 2
	}
	if strings.EqualFold(*kind, "opencode") {
		fmt.Fprintln(os.Stderr, "hostcreds: OpenCode out of scope")
		return 2
	}

	// Prefer resolved handle authority when available.
	var auth security.CredentialAuthority
	if ha, err := security.NewHandleAuthorityFromEnv(); err == nil && len(ha.Hosts()) > 0 {
		auth = ha
	}
	d := security.DiagnoseKindAuthReadinessWith(*kind, auth)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(d)
	} else {
		fmt.Println(security.FormatKindAuthBlocker(d))
		fmt.Printf("brokerable=%v class=%s integration_api=%d\n",
			d.Brokerable, d.Class, security.IntegrationAPIVersion)
	}
	if !d.Brokerable {
		return 2
	}
	return 0
}

func runHostCredsSession(args []string) int {
	fs := flag.NewFlagSet("hostcreds session", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kind := fs.String("kind", "", "author kind")
	asJSON := fs.Bool("json", false, "machine-readable summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(os.Stderr, "hostcreds session: --kind required")
		return 2
	}

	auth, err := security.NewHandleAuthorityFromEnv()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: authority: %v\n", err)
		return 1
	}
	sess, err := security.StartAuthorSessionNonInteractive(*kind, "", auth)
	if err != nil {
		var be *security.BlockedError
		if errors.As(err, &be) {
			fmt.Fprintln(os.Stderr, be.Error())
			return 2
		}
		fmt.Fprintf(os.Stderr, "hostcreds session: %v\n", err)
		return 1
	}
	defer sess.Close()

	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: %v\n", err)
		return 1
	}
	hosts := auth.Hosts()
	summary := map[string]any{
		"session_id":      sess.ID,
		"kind":            sess.Kind,
		"transport":       "https-mitm-connect",
		"proxy":           sess.Mitm != nil,
		"hosts_present":   hosts,
		"authority_class": auth.Class(),
		"durable":         auth.Durable(),
		"integration_api": security.IntegrationAPIVersion,
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
	} else {
		fmt.Printf("HOSTCREDS_SESSION session_id=%s kind=%s transport=https-mitm-connect hosts_present=%v authority=%s integration_api=%d\n",
			sess.ID, sess.Kind, hosts, auth.Class(), security.IntegrationAPIVersion)
	}
	return 0
}

func runHostCredsSelftest(args []string) int {
	fs := flag.NewFlagSet("hostcreds selftest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	vault := security.NewTestCredentialVault()
	secret := "Bearer herd-hostcreds-selftest-secret-not-for-production"
	if err := vault.InstallTestSecret("127.0.0.1", secret); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: %v\n", err)
		return 1
	}
	sess, err := security.StartHostCredsSession(security.SessionConfig{
		Kind: "fake", SessionID: "selftest-1", Authority: vault,
		AllowLoopback: true, EnableOracle: true, Interactive: false,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: start: %v\n", err)
		return 1
	}
	defer sess.Close()
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: worker env: %v\n", err)
		return 1
	}
	proof, err := security.ProveExactSessionHostCreds(sess, secret, "HOSTCREDS_SELFTEST_OK")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: FAIL %v\n", err)
		return 1
	}
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny || !proof.NoAPIKeys {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: incomplete proof\n")
		return 1
	}
	fmt.Printf("hostcreds selftest: PASS session_id=%s marker=%s no_api_keys=true integration_api=%d\n",
		proof.SessionID, proof.AllowedMarker, security.IntegrationAPIVersion)
	return 0
}
