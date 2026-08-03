package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harness"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, harness.DefaultDiscovery{})) }

func run(args []string, out, errOut io.Writer, discovery harness.HookDiscovery) int {
	fs := flag.NewFlagSet("herd-hook-inventory", flag.ContinueOnError)
	fs.SetOutput(errOut)
	provider := fs.String("provider", "claude", "provider to inspect")
	validate := fs.Bool("validate", false, "validate the discovered policy binding")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return 2
	}
	result, err := discovery.Discover(strings.TrimSpace(*provider))
	if err != nil || result.State == harness.DiscoveryFailed {
		if err != nil {
			_, _ = fmt.Fprintln(errOut, err)
		}
		return 1
	}
	if *validate && result.PolicyRequired {
		_, code, digest := harness.ApplyHookPolicies(result.Hooks, result.Policies, result.PolicyRevision)
		if code != harness.HookCodeHealthy {
			_, _ = fmt.Fprintf(errOut, "%s handler=%s\n", code, digest)
			return 1
		}
	}
	if *validate {
		_, err = fmt.Fprintf(out, "{\"provider\":%q,\"policy_revision\":%q,\"valid\":true}\n", *provider, result.PolicyRevision)
	} else {
		inventory, inventoryErr := harness.DiscoverHookPolicyInventory(discovery, *provider)
		if inventoryErr != nil {
			_, _ = fmt.Fprintln(errOut, inventoryErr)
			return 1
		}
		err = json.NewEncoder(out).Encode(inventory)
	}
	if err != nil {
		_, _ = fmt.Fprintln(errOut, err)
		return 1
	}
	return 0
}
