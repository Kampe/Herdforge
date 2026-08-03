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

// runHostCreds is the FAC-170 compiled production caller for HostCreds.
// Independent of FAC-133 WIP: diagnose readiness, start a session channel,
// or run the deterministic oracle selftest (exact-session causal proof).
//
//	herd hostcreds diagnose --kind grok
//	herd hostcreds session  --kind grok
//	herd hostcreds selftest
func runHostCreds() {
	if len(os.Args) < 3 {
		printHostCredsUsage()
		os.Exit(2)
	}
	sub := os.Args[2]
	switch sub {
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
		fmt.Fprintf(os.Stderr, "hostcreds: unknown subcommand %q\n", sub)
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

Exit codes:
  0  success / brokerable
  1  fatal error
  2  usage or typed BLOCKED (missing/unbrokerable HostCreds)

Never prints credential bytes. OpenCode is out of scope.`)
}

func runHostCredsDiagnose(args []string) int {
	fs := flag.NewFlagSet("hostcreds diagnose", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	kind := fs.String("kind", "", "author kind: grok, claude, or codex")
	asJSON := fs.Bool("json", false, "emit KindAuthDiagnosis JSON (redacted)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(os.Stderr, "hostcreds diagnose: --kind required")
		return 2
	}
	if strings.EqualFold(*kind, "opencode") {
		fmt.Fprintln(os.Stderr, "hostcreds: OpenCode out of scope (FAC-170)")
		return 2
	}

	d := security.DiagnoseKindAuthReadiness(*kind)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(d); err != nil {
			fmt.Fprintf(os.Stderr, "hostcreds diagnose: %v\n", err)
			return 1
		}
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
	kind := fs.String("kind", "", "author kind: grok, claude, or codex")
	asJSON := fs.Bool("json", false, "machine-readable session summary")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if strings.TrimSpace(*kind) == "" {
		fmt.Fprintln(os.Stderr, "hostcreds session: --kind required")
		return 2
	}
	if strings.EqualFold(*kind, "opencode") {
		fmt.Fprintln(os.Stderr, "hostcreds: OpenCode out of scope (FAC-170)")
		return 2
	}

	// Coordinator-only store from process env (never handed to worker as real keys).
	store := security.NewMemorySecretStore()
	if err := security.LoadEnvIntoStore(store); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: load store: %v\n", err)
		return 1
	}

	sess, err := security.StartAuthorSessionNonInteractive(*kind, "", store)
	if err != nil {
		var be *security.BlockedError
		if errors.As(err, &be) {
			fmt.Fprintln(os.Stderr, security.RedactSecrets(be.Error()))
			return 2
		}
		fmt.Fprintf(os.Stderr, "hostcreds session: %v\n", security.RedactSecrets(err.Error()))
		return 1
	}
	defer sess.Close()

	// Consume a non-interactive prompt (no login UI).
	if err := sess.ConsumePrompt("herd hostcreds session: non-interactive author start"); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: %v\n", err)
		return 1
	}
	if err := sess.AssertNoWorkerBearerToken(); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds session: worker channel invariant: %v\n", err)
		return 1
	}

	summary := map[string]any{
		"session_id":           sess.ID,
		"kind":                 sess.Kind,
		"channel":              "unix-oracle",
		"socket_set":           sess.Oracle != nil && sess.Oracle.SocketPath() != "",
		"prompt_consumed":      sess.PromptConsumed(),
		"hosts_creds":          sess.Oracle.CredHosts(), // names only
		"worker_has_proxy_url": false,
		"dummy_cli_sentinel":   security.DummyNeverUpstream,
		"integration_api":      security.IntegrationAPIVersion,
		// Never emit secrets or full worker env values that could hold them.
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(summary)
	} else {
		fmt.Printf("HOSTCREDS_SESSION session_id=%s kind=%s channel=unix-oracle prompt_consumed=%v hosts_creds=%v integration_api=%d\n",
			sess.ID, sess.Kind, sess.PromptConsumed(), sess.Oracle.CredHosts(), security.IntegrationAPIVersion)
	}
	return 0
}

func runHostCredsSelftest(args []string) int {
	fs := flag.NewFlagSet("hostcreds selftest", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// Deterministic in-process proof: no live provider, no FAC-133, no OpenCode.
	store := security.NewMemorySecretStore()
	secret := "Bearer herd-hostcreds-selftest-secret-not-for-production"
	if err := store.Set("api.x.ai", secret); err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: %v\n", err)
		return 1
	}
	sess, err := security.StartHostCredsSession(security.SessionConfig{
		Kind:        "fake",
		Store:       store,
		Interactive: false,
		ExtraHosts:  []string{"127.0.0.1"},
		TestRules: append(security.DefaultRequestRules(), security.RequestRule{
			Host: "127.0.0.1", Method: "POST", PathPrefix: "/v1/chat/completions", Action: "chat.completions",
		}),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: start: %v\n", err)
		return 1
	}
	defer sess.Close()

	marker := "HOSTCREDS_SELFTEST_OK"
	proof, err := security.ProveExactSessionHostCreds(sess, secret, marker)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: FAIL %v\n", security.RedactSecrets(err.Error()))
		return 1
	}
	if !proof.PromptConsumed || !proof.AllowedMarkerReach || !proof.ForbiddenAccessDeny ||
		!proof.WorkerSecretHidden || !proof.NoWorkerBearer || !proof.DummyNeverUpstream {
		fmt.Fprintf(os.Stderr, "hostcreds selftest: incomplete proof %+v\n", proof)
		return 1
	}
	// Ensure secret never printed.
	fmt.Printf("hostcreds selftest: PASS session_id=%s marker=%s integration_api=%d\n",
		proof.SessionID, proof.AllowedMarker, security.IntegrationAPIVersion)
	return 0
}
