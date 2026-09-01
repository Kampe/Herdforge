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
const controlSurfaceVersion = 26

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
		"board-audit", "candidate", "capacity", "handoffs", "utilization", "control-surface", "mail", "preflight", "preflight-static", "process", "resources", "route", "scope", "selftest", "status", "tests-for", "throughput", "timeline", "tool-probe", "transcript", "unmerged", "verify", "worktrees",
	},
	classCoordinatorOnly: {
		"activate", "approve", "attention", "board-done", "board-freeze", "board-frozen", "board-sync", "claude-only", "cleanup", "command", "commands", "containers", "control", "daemon", "deps", "dispatch", "doctor-models", "drain", "feedback", "fence-provision", "finish", "forge", "fresh-build", "goal-guard", "harvest", "harvest-merge", "hooks-pin", "herdr-deliver", "hold", "kick", "labels", "lane-cut", "worktree-reap", "legacy-receipts", "lifecycle", "lock", "lost", "merge-admit", "merge-complete", "remote-ci-settle", "next", "no-claude", "overlap", "park", "posture", "pool", "pulse", "quota", "quota-supervisor", "receipt", "repl", "rescue", "reset-safe", "resolve-lane", "review", "review-host", "integrate", "review-classify", "review-ingest", "launch-record", "review-ledger", "verdict-harvest", "verdict-push", "send", "sh", "shoot", "shot", "slot", "spin", "standing", "stop", "task", "up", "usage", "watch", "wave", "wind-down", "verify-fac151",
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
		"board-audit": true, "board-frozen": true, "capacity": true, "utilization": true, "control-surface": true, "preflight": true, "preflight-static": true,
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
	26: "9192e793a775fdb5e71916c93af36097285092de9f0f70d3f87b45cea2e81099",
	1:  "ec8bcd82bb03cc6e33e6a87515cbd9236aa997a2efca802f5d800b8ba0afe121",
	2:  "96f0e6e4ef2653c583b4580efe3ea5d5b6d537dfc8282e2eda62c0905dcd5287",
	3:  "d2e75d61dec1d2cd0dac05dac7b3515ef6d7f113a52a08d8e0735447e040be80",
	4:  "2383e2f7e6de2ebeff22f7d042cc13895be1016e46a5f1726b97b8b235c851b3",
	5:  "c784289659d0339bf5b5b418473fb4595ab8ef1d8cb73416f474e628bfdf5a25",
	6:  "d1f5c427cf2144bac25bd6aaa31d309da9a01978b12b385ad75e53b9ea7b50d5",
	7:  "8213c48a046f7686baac098199f9a1f5ac345d061882fc229535089a9e185355",
	8:  "ac11d8f8a90c9197fe927ae9dfa8790f49d5b3f8f57691ae5a27d252026a5331",
	9:  "a0065bf1cd7871a1ad75cd79cacb7c9ef40222978c36dfffae53d24ccd340c54",
	10: "081f52ef2421bbe00e12b839d4e9d2ccc36298106f3c9876c73424ec8484e5b9",
	// 11 adds the read-only `transcript` command (FAC-551).
	11: "b51d4a5b08dd8a89c4ce71d12024b107dc311f6be0aa060622e8ef87cb7ff3e2",
	// 12 adds the read-only `candidate` identity command (FAC-568).
	12: "b71cf3414849949c33a93f940b5f5e4f053bf17f2e919c2b9a52e47f037a13b9",
	// 13 adds the read-only `handoffs` queue command (FAC-569).
	13: "bc787445e6e85201ac7f2da06852a4c62a96b12498f16329b7515dad92e5da99",
	// 14 registers `pool`, whose switch case existed but was never admitted
	// (FAC-573): warm-pool leases could be taken and never released.
	14: "175e34fc0291a5679d68aa11d7bc584c3b1061287b23da5aad7c502b4cedeba9",
	// 15 also registers `finish`, likewise unreachable (FAC-573).
	15: "05e09fdccaf952a41518b354ed526c3e2496b3b4634fce27b47b0bc47d86a454",
	// FAC-594 adds hooks-pin: without it a stale hook policy grounds every
	// standing launch and there is no command to repair the pin.
	16: "75961fe828bdb14800f2dbaa3ad17cc1c1b0bff075e14b0de3fd785e8b46979b",
	// FAC-619: adds verdict-push, the git transport a reviewer runs to send its
	// verdict to the ledger host.
	17: "5a9d0d90a8792e9998eb6502beb76cc75afd40e17bf1184751842ea3007573fc",
	// FAC-621: adds verdict-harvest, the collecting half of the git transport.
	18: "c6ba20f4478421e5a7f4616ef759a1b82a610bd27003028e2af922423c42786f",
	// FAC-637: adds launch-record, the write that makes builder provenance
	// provable instead of asking reviewers to attest what nobody recorded.
	19: "a6e871116aeb1e334585616784353260c0c8d3528e13017e5298a14040ea0ba4",
	// FAC-655: adds lane-cut, which extracts a bounded candidate from a
	// long-lived standing-lane branch onto a fresh branch from origin/main.
	// Mutating: it creates a worktree and a branch. It never writes to the
	// lane's own branch.
	20: "504da2f6281b412b1030e712081f43375b8ee47d5183c60daaf68fd29e5c9144",
	// FAC-672: adds worktree-reap, the retirement half of the worktree
	// lifecycle. Mutating: it removes worktrees and fully-merged branches, and
	// only ever those whose commits are already in the base.
	25: "7374672155120b8bfe30ac4adacb285131200a35c7a1ff5d8b72d8282314aaaa",
	24: "43da5040001f90b6991097bfb02cd59f42bf5dc3a5bde3a04b340ce26a59acf0",
	23: "3db3c11d901f0c11c2fbd02cacd798bdf42c46d5734273fbabb4a656bc3d3b7f",
	22: "24076bdc754b4420b31545c3061f970144b0b3a7df087f0604379038f40d7c66",
	21: "5ae7dd9d1262233a1a5ea55f813adbbcccfca75fab2b5718977f2bbf1fc06fc7",
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
