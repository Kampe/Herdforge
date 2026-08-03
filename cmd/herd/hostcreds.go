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
	fmt.Fprintln(os.Stderr, `herd hostcreds — HostCreds oracle (FAC-170)

Usage:
  herd hostcreds diagnose --kind <grok|claude|codex>
  herd hostcreds session  --kind <grok|claude|codex>
  herd hostcreds selftest

Production authority: HERD_HOSTCREDS_HANDLES="api.x.ai=keychain:…;…"
  (keychain: or op:// handles only — never raw API keys / HERD_HOST_CREDS)

Exit: 0 ok, 1 fatal, 2 BLOCKED/usage. Never prints credential bytes. No OpenCode.`)
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

	if err := sess.ConsumePrompt("herd hostcreds session: non-interactive"); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: %v\n", err)
		return 1
	}
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: %v\n", err)
		return 1
	}

	summary := map[string]any{
		"session_id":      sess.ID,
		"kind":            sess.Kind,
		"channel":         "unix-oracle",
		"prompt_consumed": sess.PromptConsumed(),
		"hosts_present":   sess.Oracle.CredHosts(),
		"authority_class": auth.Class(),
		"durable":         auth.Durable(),
		"integration_api": security.IntegrationAPIVersion,
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
	} else {
		fmt.Printf("HOSTCREDS_SESSION session_id=%s kind=%s channel=unix-oracle hosts_present=%v authority=%s integration_api=%d\n",
			sess.ID, sess.Kind, sess.Oracle.CredHosts(), auth.Class(), security.IntegrationAPIVersion)
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
	if err := vault.InstallTestSecret("api.x.ai", secret); err != nil {
		// fake kind does not need api.x.ai; install loopback in proof
		_ = err
	}
	if err := vault.InstallTestSecret("127.0.0.1", secret); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: %v\n", err)
		return 1
	}

	sess, err := security.StartHostCredsSession(security.SessionConfig{
		Kind:          "fake",
		Authority:     vault,
		Interactive:   false,
		AllowLoopback: true,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: start: %v\n", err)
		return 1
	}
	defer sess.Close()

	proof, err := security.ProveExactSessionHostCreds(sess, secret, "HOSTCREDS_SELFTEST_OK")
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: FAIL %v\n", err)
		return 1
	}
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny ||
		!proof.WorkerSecretHidden || !proof.NoWorkerBearer || !proof.DummyNeverUpstream || !proof.NoSecretExportAPI {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: incomplete proof\n")
		return 1
	}
	fmt.Printf("hostcreds selftest: PASS session_id=%s marker=%s integration_api=%d\n",
		proof.SessionID, proof.AllowedMarker, security.IntegrationAPIVersion)
	return 0
}
