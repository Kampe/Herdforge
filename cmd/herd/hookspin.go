package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Kampe/Herdforge/pkg/harness"
)

// hooksPinFile is the on-disk shape of .herd/harness-hooks.json. `revision` is
// derived by FileDiscovery from the policy set, so it is deliberately not
// written here: emitting a stale one would fail discovery outright.
type hooksPinFile struct {
	Providers map[string]hooksPinProvider `json:"providers"`
}

type hooksPinProvider struct {
	Hooks               []harness.Hook       `json:"hooks"`
	ApprovedAuthorities []string             `json:"approved_local_authorities"`
	Policies            []harness.HookPolicy `json:"policies"`
}

// runHooksPin refreshes the pinned hook policy set against the harness's LIVE
// hooks.
//
// FAC-594: there was no way to do this. When a ~/.claude hook changes, its
// digest changes, and the pin keeps naming a handler that no longer exists.
// ApplyHookPolicies refuses any policy matching no hook, so a single stale entry
// grounds every standing launch with hook.policy_mismatch — observed here as 29
// pinned policies against 27 live hooks, with orphans claude:lifecycle:324eab26
// and claude:lifecycle:c5d8de5a taking down 5 of 6 standing roles.
//
// That mattered beyond the outage: with the sanctioned path refusing to launch,
// agents hand-rolled `herdr agent start --kind claude`, which silently resolves
// to a low-tier model and burned real spend. A broken safe path pushes work onto
// the unsafe one, so this command exists to keep the safe path usable.
func runHooksPin(argv []string) error {
	fs := flag.NewFlagSet("hooks-pin", flag.ContinueOnError)
	provider := fs.String("provider", "claude", "Harness provider to pin")
	path := fs.String("file", filepath.Join(".herd", "harness-hooks.json"), "Pin file to write")
	dryRun := fs.Bool("dry-run", false, "Report the delta without writing")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	live, err := harness.DefaultDiscovery{}.Discover(*provider)
	if err != nil {
		return fmt.Errorf("discover live hooks for %s: %w", *provider, err)
	}
	if live.State != harness.DiscoveryHooks {
		return fmt.Errorf("provider %s discovered no hooks (state %q); refusing to pin an empty policy set",
			*provider, live.State)
	}

	// Preserve the operator's existing classifications. A refresh must not
	// silently re-classify a hook the operator deliberately marked.
	prior := map[string]harness.HookPolicy{}
	file := hooksPinFile{Providers: map[string]hooksPinProvider{}}
	if body, readErr := os.ReadFile(*path); readErr == nil {
		if err := json.Unmarshal(body, &file); err != nil {
			return fmt.Errorf("read existing pin %s: %w", *path, err)
		}
		for _, p := range file.Providers[strings.ToLower(*provider)].Policies {
			prior[p.HandlerDigest] = p
		}
	} else if !os.IsNotExist(readErr) {
		return fmt.Errorf("stat existing pin %s: %w", *path, readErr)
	}
	if file.Providers == nil {
		file.Providers = map[string]hooksPinProvider{}
	}

	liveDigests := map[string]bool{}
	policies := make([]harness.HookPolicy, 0, len(live.Hooks))
	var kept, added []string
	for _, hook := range live.Hooks {
		liveDigests[hook.Name] = true
		if p, ok := prior[hook.Name]; ok {
			policies = append(policies, p)
			kept = append(kept, hook.Name)
			continue
		}
		// A NEW hook is pinned optional with no health URL. Its discovered
		// requirement may be "required", and a required policy without a health
		// URL makes ApplyHookPolicies return NoHealth, which would ground the
		// fleet exactly as the stale orphans did. Classifying it explicitly is
		// the operator's call; defaulting to optional keeps launches working and
		// is reported below rather than done silently.
		policies = append(policies, harness.HookPolicy{
			HandlerDigest: hook.Name,
			Requirement:   harness.HookOptional,
			HealthURL:     "",
		})
		added = append(added, hook.Name)
	}

	var dropped []string
	for digest := range prior {
		if !liveDigests[digest] {
			dropped = append(dropped, digest)
		}
	}
	sort.Strings(dropped)
	sort.Strings(added)
	sort.Slice(policies, func(i, j int) bool { return policies[i].HandlerDigest < policies[j].HandlerDigest })

	entry := file.Providers[strings.ToLower(*provider)]
	entry.Policies = policies
	if entry.Hooks == nil {
		entry.Hooks = []harness.Hook{}
	}
	if entry.ApprovedAuthorities == nil {
		entry.ApprovedAuthorities = []string{}
	}
	file.Providers[strings.ToLower(*provider)] = entry

	for _, d := range dropped {
		fmt.Printf("DROP   %s (no live hook matches this digest)\n", d)
	}
	for _, d := range added {
		fmt.Printf("ADD    %s requirement=optional (classify explicitly if it must gate)\n", d)
	}
	fmt.Printf("hooks-pin: provider=%s live=%d kept=%d added=%d dropped=%d revision=%s\n",
		*provider, len(live.Hooks), len(kept), len(added), len(dropped),
		harness.HookPolicyRevision(policies))

	if *dryRun {
		fmt.Println("hooks-pin: dry run, nothing written")
		return nil
	}

	// Prove the refreshed set actually binds before replacing the file. Writing
	// a pin that still fails ApplyHookPolicies would leave the fleet grounded
	// with a file that looks freshly repaired.
	if _, code, digest := harness.ApplyHookPolicies(live.Hooks, policies, harness.HookPolicyRevision(policies)); code != harness.HookCodeHealthy {
		return fmt.Errorf("refreshed policy set still does not bind: %s handler=%s", code, digest)
	}

	body, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	tmp := *path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, *path); err != nil {
		return fmt.Errorf("install %s: %w", *path, err)
	}
	fmt.Printf("hooks-pin: wrote %s\n", *path)
	return nil
}
