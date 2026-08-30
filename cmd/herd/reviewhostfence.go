package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/provenance"
	"github.com/Kampe/Herdforge/pkg/reviewlaunch"
)

var (
	reviewHostNameRE        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	reviewHostRemoteHerdRE  = regexp.MustCompile(`^(?:[A-Za-z0-9][A-Za-z0-9._-]*|\$HOME/[A-Za-z0-9._/-]+|/[A-Za-z0-9._/-]+)$`)
	reviewHostCommandRE     = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	reviewHostVersionCheck  = checkReviewHostVersion
	reviewHostLocalRevision = currentHerdRevision
	reviewHostRemoteCommand = runRemoteHerdCommand
)

type reviewHostVersionEvidence struct {
	Host            string
	RequiredCommand string
	LocalRevision   string
	RemoteRevision  string
}

// runReviewHostFence is the circuit breaker for a remote review host.
//
// WHY THIS EXISTS
// ---------------
// FAC-686. A review supervisor launched four concurrent reviews onto W4
// (#3400, #3399, #3398, #3397), attempted a fifth, and kept probing over ssh as
// the host degraded. W4 then stopped completing an ssh banner exchange. Every
// individual action was reasonable; nothing was watching the aggregate, so the
// fleet kept pushing on a host that had stopped answering.
//
// FAC-683 gave the fleet a gate that says "yes". This is the missing half: a
// thing that says STOP and stays stopped. One banner or control-plane timeout
// fences the host for every subsequent launch, and only an EXPLICIT recovery
// event clears it.
//
// The fence does NOT expire on a timer. That is deliberate and it is the
// difference between this and a retry backoff: a host that fell over under fleet
// pressure will look fine the moment the pressure stops, so a self-clearing
// fence would re-admit the same storm. Clearing it is an operator statement that
// someone looked at the host, and `--recover` requires the evidence to say so.
func runReviewHostFence(args []string) error {
	fs := flag.NewFlagSet("review-host", flag.ContinueOnError)
	host := fs.String("host", envOr("HERD_REVIEW_HOST", "wsl-box"), "review host this fence covers")
	trip := fs.String("fence", "", "fence the host: the reason it stopped answering")
	recover := fs.Bool("recover", false, "clear the fence; requires --evidence")
	evidence := fs.String("evidence", "", "what was actually inspected on the host before recovering it")
	checkVersion := fs.Bool("check-version", false, "verify remote herd revision and required subcommand before dispatch")
	requiredCommand := fs.String("require-command", "capacity", "remote herd subcommand the launcher requires")
	remoteHerd := fs.String("remote-herd", "herd", "remote herd executable (name or absolute/$HOME path)")
	timeout := fs.Duration("timeout", 15*time.Second, "bounded remote version check timeout")
	asJSON := fs.Bool("json", false, "emit the structured fence record")
	if err := fs.Parse(args); err != nil {
		return err
	}

	switch {
	case strings.TrimSpace(*trip) != "":
		f := hostFence{Host: *host, Reason: strings.TrimSpace(*trip), FencedAt: time.Now().UTC().Format(time.RFC3339)}
		if err := writeHostFence(f); err != nil {
			return err
		}
		fmt.Printf("FENCED %s: %s\n", f.Host, f.Reason)
		fmt.Printf("  No further launches at %s until `herd review-host --recover --evidence <what you checked>`.\n", f.Host)
		return nil

	case *recover:
		// A recovery with no evidence is indistinguishable from an impatient
		// retry, and an impatient retry is what produced the incident.
		if strings.TrimSpace(*evidence) == "" {
			return fmt.Errorf("--recover requires --evidence: name what was inspected on %s (memory, top RSS, OOM/journal, ssh, herdr, agent roster). "+
				"Clearing a fence is a statement that someone looked", *host)
		}
		f, ok, err := readHostFence(*host)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Printf("%s is not fenced; nothing to recover\n", *host)
			return nil
		}
		if err := clearHostFence(*host); err != nil {
			return err
		}
		fmt.Printf("RECOVERED %s (was fenced %s: %s)\n  evidence: %s\n", *host, f.FencedAt, f.Reason, strings.TrimSpace(*evidence))
		return nil

	case *checkVersion:
		// Honor the circuit breaker before contacting a host. The version check
		// is the first remote launch gate and must not become a probe storm.
		f, fenced, err := readHostFence(*host)
		if err != nil {
			return err
		}
		if fenced {
			return fmt.Errorf("review host %s is fenced since %s: %s", *host, f.FencedAt, f.Reason)
		}
		if *timeout <= 0 {
			return errors.New("review-host: --timeout must be positive")
		}
		ctx, cancel := context.WithTimeout(context.Background(), *timeout)
		defer cancel()
		proof, err := reviewHostVersionCheck(ctx, *host, *remoteHerd, *requiredCommand)
		if err != nil {
			return err
		}
		fmt.Printf("CURRENT %s required_command=%s local_revision=%s remote_revision=%s\n",
			proof.Host, proof.RequiredCommand, proof.LocalRevision, proof.RemoteRevision)
		return nil
	}

	// Default: report status, and exit non-zero while fenced so a shell caller
	// gating a launch on it cannot proceed by ignoring the text.
	f, fenced, err := readHostFence(*host)
	if err != nil {
		return err
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(struct {
			hostFence
			Fenced bool `json:"fenced"`
		}{f, fenced}); err != nil {
			return err
		}
	} else if fenced {
		fmt.Printf("FENCED %s since %s: %s\n", f.Host, f.FencedAt, f.Reason)
	} else {
		fmt.Printf("OPEN %s\n", *host)
	}
	if fenced {
		os.Exit(3)
	}
	return nil
}

func checkReviewHostVersion(ctx context.Context, host, remoteHerd, requiredCommand string) (reviewHostVersionEvidence, error) {
	host = strings.TrimSpace(host)
	remoteHerd = strings.TrimSpace(remoteHerd)
	requiredCommand = strings.TrimSpace(requiredCommand)
	if !reviewHostNameRE.MatchString(host) {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: unsafe host %q", host)
	}
	if !reviewHostRemoteHerdRE.MatchString(remoteHerd) {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: unsafe remote herd executable %q", remoteHerd)
	}
	if !reviewHostCommandRE.MatchString(requiredCommand) {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: unsafe required command %q", requiredCommand)
	}
	localRevision, err := reviewHostLocalRevision()
	if err != nil {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: local herd revision: %w", err)
	}
	localRevision = strings.TrimSpace(localRevision)
	if localRevision == "" {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: local herd revision is unknown; refusing an unprovable remote dispatch")
	}

	versionOut, versionErr := reviewHostRemoteCommand(ctx, host, remoteHerd, "--version")
	if versionErr != nil {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: read remote herd revision from %s: %w: %s", host, versionErr, strings.TrimSpace(string(versionOut)))
	}
	remoteRevision, err := reviewlaunch.ParseHerdVersion(string(versionOut))
	if err != nil {
		return reviewHostVersionEvidence{}, fmt.Errorf("review-host: %s: %w", host, err)
	}
	requirement := reviewlaunch.VersionRequirement{
		RequiredCommand: requiredCommand,
		LocalRevision:   localRevision,
		RemoteRevision:  remoteRevision,
	}
	// Compare before invoking the command the launcher depends on. This is the
	// fail-closed boundary that keeps a stale binary from reaching capacity
	// claim, worktree preparation, lease creation, or tab creation.
	if err := reviewlaunch.RequireRemoteHerd(requirement, "", nil); err != nil {
		return reviewHostVersionEvidence{}, err
	}
	commandOut, commandErr := reviewHostRemoteCommand(ctx, host, remoteHerd, requiredCommand, "--help")
	if err := reviewlaunch.RequireRemoteHerd(requirement, string(commandOut), commandErr); err != nil {
		return reviewHostVersionEvidence{}, err
	}
	return reviewHostVersionEvidence{Host: host, RequiredCommand: requiredCommand, LocalRevision: localRevision, RemoteRevision: remoteRevision}, nil
}

func currentHerdRevision() (string, error) {
	local, err := provenance.CurrentExecutable()
	if err != nil {
		return "", err
	}
	return local.BinaryRevision, nil
}

func runRemoteHerdCommand(ctx context.Context, host, remoteHerd string, args ...string) ([]byte, error) {
	sshArgs := []string{"-o", "ConnectTimeout=15", host, remoteHerd}
	sshArgs = append(sshArgs, args...)
	return exec.CommandContext(ctx, "ssh", sshArgs...).CombinedOutput()
}

type hostFence struct {
	Host     string `json:"host"`
	Reason   string `json:"reason,omitempty"`
	FencedAt string `json:"fenced_at,omitempty"`
}

// hostFenceDir is per-user and outside any checkout: a fenced host is a property
// of the machine, not of a repository, and two repositories must not disagree
// about whether it is safe to launch there.
func hostFenceDir() string {
	if p := strings.TrimSpace(os.Getenv("HERD_HOST_FENCE_DIR")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "herd-host-fences")
	}
	return filepath.Join(home, ".herd", "state", "host-fences")
}

func hostFencePath(host string) string {
	return filepath.Join(hostFenceDir(), safeReviewSurfacePart(host)+".json")
}

func writeHostFence(f hostFence) error {
	if err := os.MkdirAll(hostFenceDir(), 0o700); err != nil {
		return err
	}
	b, err := json.Marshal(f)
	if err != nil {
		return err
	}
	return os.WriteFile(hostFencePath(f.Host), append(b, '\n'), 0o600)
}

// readHostFence reports whether the host is fenced. An unreadable or malformed
// fence file counts as FENCED: the file exists to stop launches, so failing to
// parse it must not be the thing that lets one through.
func readHostFence(host string) (hostFence, bool, error) {
	raw, err := os.ReadFile(hostFencePath(host))
	if os.IsNotExist(err) {
		return hostFence{Host: host}, false, nil
	}
	if err != nil {
		return hostFence{Host: host, Reason: "fence file unreadable: " + err.Error()}, true, nil
	}
	var f hostFence
	if err := json.Unmarshal(raw, &f); err != nil {
		return hostFence{Host: host, Reason: "fence file malformed; treating as fenced"}, true, nil
	}
	f.Host = host
	return f, true, nil
}

func clearHostFence(host string) error {
	err := os.Remove(hostFencePath(host))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
