package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/Kampe/Herdforge/pkg/provider"
)

// runFenceProvision first-time (or rotate) seals the shared claim volume.
// Operators must unset HERD_FENCE_VOLUME_ID for first provision; the command
// prints the minted seal for fleet distribution.
//
//	HERD_CLAIM_DIR=/shared/herd-claim herd fence-provision
//	HERD_CLAIM_DIR=... HERD_FENCE_VOLUME_ID=<existing> HERD_FENCE_ROTATE=1 herd fence-provision
func runFenceProvision() {
	claimDir := os.Getenv("HERD_CLAIM_DIR")
	if claimDir == "" {
		fmt.Fprintf(os.Stderr, "herd fence-provision: HERD_CLAIM_DIR is required (shared claim volume path)\n")
		os.Exit(1)
	}
	abs, err := filepath.Abs(claimDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-provision: %v\n", err)
		os.Exit(1)
	}
	if err := os.Setenv("HERD_CLAIM_DIR", abs); err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-provision: set HERD_CLAIM_DIR: %v\n", err)
		os.Exit(1)
	}

	rotate := os.Getenv("HERD_FENCE_ROTATE") == "1"
	if !rotate {
		// First-time path: WriteSharedMarker refuses a pre-set volume id so
		// hosts cannot plant a stolen seal. Clear any ambient value.
		_ = os.Unsetenv("HERD_FENCE_VOLUME_ID")
		_ = os.Unsetenv("HERD_FENCE_PROVISION_TOKEN")
		if err := os.Setenv("HERD_FENCE_PROVISION", "1"); err != nil {
			fmt.Fprintf(os.Stderr, "herd fence-provision: set HERD_FENCE_PROVISION: %v\n", err)
			os.Exit(1)
		}
	}

	if err := provider.WriteSharedMarker(abs); err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-provision: %v\n", err)
		os.Exit(1)
	}
	seal, err := provider.ReadVolumeSeal(abs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-provision: read seal: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("herd fence-provision: sealed claim volume at %s\n", abs)
	fmt.Println("Distribute to every fleet host (env only — never commit the seal):")
	fmt.Printf("  export HERD_CLAIM_DIR=%q\n", abs)
	fmt.Printf("  export HERD_FENCE_VOLUME_ID=%q\n", seal)
	fmt.Println("Then start the broker once per shared volume:")
	fmt.Println("  HERD_FENCE_BROKER_TOKEN=<worker-tok-min-16>")
	fmt.Println("  HERD_FENCE_BROKER_MINT_TOKEN=<mint-tok-min-16-distinct>")
	fmt.Println("  herd fence-broker")
}

// runFenceBroker starts the FAC-147 fence broker sidecar (exclusive flock on
// the claim volume). Blocks until SIGINT/SIGTERM.
//
// Required env:
//
//	HERD_CLAIM_DIR, HERD_FENCE_VOLUME_ID, HERD_FENCE_BROKER_TOKEN,
//	HERD_FENCE_BROKER_MINT_TOKEN
//
// Optional:
//
//	HERD_FENCE_BROKER_LISTEN (default unix socket under claim dir)
//	KANEO_API_URL / project via herd.yaml-free upstream URL env used by StartFenceBroker
func runFenceBroker() {
	claimDir := os.Getenv("HERD_CLAIM_DIR")
	if claimDir == "" {
		fmt.Fprintf(os.Stderr, "herd fence-broker: HERD_CLAIM_DIR is required\n")
		os.Exit(1)
	}
	token := os.Getenv("HERD_FENCE_BROKER_TOKEN")
	mint := os.Getenv("HERD_FENCE_BROKER_MINT_TOKEN")
	listen := os.Getenv("HERD_FENCE_BROKER_LISTEN")
	if listen == "" {
		listen = "unix"
	}
	upstreamURL := os.Getenv("KANEO_API_URL")
	if upstreamURL == "" {
		upstreamURL = os.Getenv("HERD_FENCE_UPSTREAM_URL")
	}
	upstreamProject := os.Getenv("KANEO_PROJECT_ID")
	if upstreamProject == "" {
		upstreamProject = os.Getenv("HERD_FENCE_UPSTREAM_PROJECT")
	}
	useCLI := os.Getenv("HERD_FENCE_UPSTREAM_CLI") == "1"

	cfg := provider.FenceBrokerConfig{
		ClaimDir:        claimDir,
		ListenAddr:      listen,
		Token:           token,
		MintToken:       mint,
		UpstreamURL:     upstreamURL,
		UpstreamProject: upstreamProject,
		UpstreamCLI:     useCLI,
	}
	b, err := provider.StartFenceBroker(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd fence-broker: %v\n", err)
		os.Exit(1)
	}
	defer func() { _ = b.Close() }()

	base := b.ClientBaseURL()
	if sock := b.UnixSocket(); sock != "" {
		base = "unix://" + sock
	}
	fmt.Printf("herd fence-broker: listening on %s\n", base)
	fmt.Println("Workers:")
	fmt.Printf("  export HERD_FENCE_BROKER_URL=%q\n", base)
	fmt.Printf("  export HERD_FENCE_BROKER_TOKEN=%q\n", token)
	fmt.Println("Coordinator mint secret is file-backed under the claim dir (not worker env).")

	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	sig := <-ch
	fmt.Printf("herd fence-broker: shutting down on %s\n", sig)
}
