package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

// FAC-240: controlSurfaceVersion is a compatibility boundary, not a display
// number. Any command-contract change must add its new fingerprint below and
// increment this value; ValidateControlSurfaceManifest rejects silent drift.
const controlSurfaceVersion = 6

type commandClass string

const (
	classPublicAgent     commandClass = "public-agent"
	classCoordinatorOnly commandClass = "coordinator-only"
	classOperatorOnly    commandClass = "operator-only"
	classInternal        commandClass = "internal"
)

// commandContract is the machine-readable contract for exactly one routed
// herd command. It deliberately describes the existing authority boundary;
// it does not replace command-specific authorities such as cmdauth.
type commandContract struct {
	Command        string       `json:"command"`
	Classification commandClass `json:"classification"`
	Audience       []string     `json:"audience"`
	Roles          []string     `json:"roles"`
	Mutates        bool         `json:"mutates"`
	Evidence       []string     `json:"evidence"`
	Input          string       `json:"input"`
	Output         string       `json:"output"`
}

type controlSurfaceManifest struct {
	Version  int               `json:"version"`
	Commands []commandContract `json:"commands"`
}

// commandNamesByClass is the complete CLI registry. Keep each command in one
// list only: validation rejects omissions and duplicate classifications.
var commandNamesByClass = map[commandClass][]string{
	classPublicAgent: {
		"board-audit", "control-surface", "mail", "preflight", "preflight-static", "process", "resources", "route", "scope", "selftest", "status", "tests-for", "throughput", "timeline", "tool-probe", "unmerged", "verify", "worktrees",
	},
	classCoordinatorOnly: {
		"activate", "approve", "attention", "board-done", "board-freeze", "board-frozen", "board-sync", "claude-only", "cleanup", "command", "commands", "containers", "control", "daemon", "deps", "dispatch", "doctor-models", "drain", "feedback", "fence-provision", "forge", "fresh-build", "harvest", "harvest-merge", "herdr-deliver", "hold", "kick", "labels", "lifecycle", "lock", "lost", "merge-admit", "merge-complete", "next", "no-claude", "overlap", "park", "posture", "pulse", "quota", "quota-supervisor", "receipt", "repl", "rescue", "reset-safe", "resolve-lane", "review", "review-classify", "review-ingest", "review-ledger", "send", "sh", "shoot", "shot", "spin", "standing", "stop", "task", "up", "usage", "watch", "wave", "wind-down", "verify-fac151",
	},
	classOperatorOnly: {
		"clone", "envplan", "hostcreds", "init", "seed-lane-state", "signer-boundary", "stash", "validate-config",
	},
	classInternal: {
		"broker", "fence-broker", "netbroker-serve", "role-inject",
	},
}

func commandMutates(name string) bool {
	readOnly := map[string]bool{
		"board-audit": true, "board-frozen": true, "control-surface": true, "preflight": true, "preflight-static": true,
		"process": true, "resources": true, "route": true, "scope": true, "selftest": true,
		"status": true, "tests-for": true, "throughput": true, "tool-probe": true,
		"timeline": true, "unmerged": true, "verify": true, "worktrees": true, "usage": true,
	}
	return !readOnly[name]
}

func contractFor(name string, class commandClass) commandContract {
	c := commandContract{
		Command: name, Classification: class, Mutates: commandMutates(name),
		Input:  "CLI flags and positional arguments documented by herd " + name,
		Output: "stdout result and non-zero exit on contract failure",
	}
	switch class {
	case classPublicAgent:
		c.Audience, c.Roles = []string{"agent"}, []string{"worker", "reviewer", "verification-gate"}
	case classCoordinatorOnly:
		c.Audience, c.Roles = []string{"coordinator"}, []string{"coordinator", "orchestrator"}
	case classOperatorOnly:
		c.Audience, c.Roles = []string{"operator"}, []string{"operator"}
	case classInternal:
		c.Audience, c.Roles = []string{"internal"}, []string{"herd-runtime"}
	}
	if c.Mutates {
		c.Evidence = []string{"durable receipt or command-specific authorization"}
	} else {
		c.Evidence = []string{"none: read-only result"}
	}
	return c
}

func controlSurface() controlSurfaceManifest {
	commands := make([]commandContract, 0, 96)
	for class, names := range commandNamesByClass {
		for _, name := range names {
			commands = append(commands, contractFor(name, class))
		}
	}
	sort.Slice(commands, func(i, j int) bool { return commands[i].Command < commands[j].Command })
	return controlSurfaceManifest{Version: controlSurfaceVersion, Commands: commands}
}

// Fingerprints are deliberately pinned by manifest version. Adding, removing,
// or reclassifying a command changes the hash and fails tests until the author
// explicitly increments controlSurfaceVersion and records the new hash.
var controlSurfaceCompatibility = map[int]string{
	1: "ec8bcd82bb03cc6e33e6a87515cbd9236aa997a2efca802f5d800b8ba0afe121",
	2: "96f0e6e4ef2653c583b4580efe3ea5d5b6d537dfc8282e2eda62c0905dcd5287",
	3: "d2e75d61dec1d2cd0dac05dac7b3515ef6d7f113a52a08d8e0735447e040be80",
	4: "2383e2f7e6de2ebeff22f7d042cc13895be1016e46a5f1726b97b8b235c851b3",
	5: "c784289659d0339bf5b5b418473fb4595ab8ef1d8cb73416f474e628bfdf5a25",
	6: "e23d5531ac1723c79d6c225c5b838490dfc3d4d118ccca05644119157c74bc24",
}

func controlSurfaceFingerprint(m controlSurfaceManifest) (string, error) {
	body, err := json.Marshal(m.Commands)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

func validateControlSurfaceManifest(m controlSurfaceManifest) error {
	if m.Version < 1 {
		return errors.New("control surface: manifest version must be positive")
	}
	seen := make(map[string]commandClass, len(m.Commands))
	for _, c := range m.Commands {
		if strings.TrimSpace(c.Command) == "" || c.Classification == "" || len(c.Audience) == 0 || len(c.Roles) == 0 || len(c.Evidence) == 0 || c.Input == "" || c.Output == "" {
			return fmt.Errorf("control surface: incomplete contract for %q", c.Command)
		}
		if prior, ok := seen[c.Command]; ok {
			return fmt.Errorf("control surface: %q classified more than once (%s, %s)", c.Command, prior, c.Classification)
		}
		seen[c.Command] = c.Classification
	}
	fingerprint, err := controlSurfaceFingerprint(m)
	if err != nil {
		return fmt.Errorf("control surface: fingerprint: %w", err)
	}
	if want, ok := controlSurfaceCompatibility[m.Version]; !ok || want != fingerprint {
		return fmt.Errorf("control surface: contract changed without an explicit manifest version bump (version=%d fingerprint=%s)", m.Version, fingerprint)
	}
	return nil
}

func commandContractFor(name string) (commandContract, bool) {
	for _, c := range controlSurface().Commands {
		if c.Command == name {
			return c, true
		}
	}
	return commandContract{}, false
}

// admitRoutedCommand makes the manifest the authority boundary for main's
// command switch. A new switch case cannot reach a handler until it is first
// declared in the manifest (and therefore participates in compatibility and
// discovery validation).
//
// The control-surface argument gate intentionally runs before manifest
// validation so unavailable discovery requests cause no filesystem or process
// side effects.
func admitRoutedCommand(command string, args []string) error {
	if _, known := commandContractFor(command); !known {
		return fmt.Errorf("unknown subcommand %q", command)
	}
	if command == "control-surface" && !controlSurfaceArgsAllowed(args) {
		return errors.New("control-surface: only --json is supported")
	}
	if err := validateControlSurfaceManifest(controlSurface()); err != nil {
		return fmt.Errorf("control surface: %w", err)
	}
	return nil
}

func knownSubcommands() []string {
	m := controlSurface()
	names := make([]string, 0, len(m.Commands))
	for _, c := range m.Commands {
		names = append(names, c.Command)
	}
	return names
}

func controlSurfaceArgsAllowed(args []string) bool {
	for _, arg := range args {
		switch arg {
		case "--json", "--help", "-h":
		default:
			return false
		}
	}
	return true
}

// runControlSurface intentionally publishes only public-agent contracts. The
// full registry remains compiled for local conformance tests, but an agent
// cannot discover coordinator/operator/internal operations through this API.
func runControlSurface() {
	if !controlSurfaceArgsAllowed(os.Args[2:]) {
		fmt.Fprintln(os.Stderr, "control-surface: only --json is supported")
		os.Exit(2)
	}
	m := controlSurface()
	if err := validateControlSurfaceManifest(m); err != nil {
		fmt.Fprintf(os.Stderr, "control-surface: %v\n", err)
		os.Exit(1)
	}
	public := controlSurfaceManifest{Version: m.Version}
	for _, c := range m.Commands {
		if c.Classification == classPublicAgent {
			public.Commands = append(public.Commands, c)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(public); err != nil {
		fmt.Fprintf(os.Stderr, "control-surface: encode: %v\n", err)
		os.Exit(1)
	}
}
