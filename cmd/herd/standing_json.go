package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/standing"
)

// standingJSONReport wraps the run report with the surrounding facts a consumer
// otherwise had to scrape from separate prose lines.
type standingJSONReport struct {
	Result *standing.Result `json:"result"`
	Broker *brokerJSON      `json:"broker,omitempty"`
}

type brokerJSON struct {
	Serving bool   `json:"serving"`
	Socket  string `json:"socket,omitempty"`
	Detail  string `json:"detail,omitempty"`
}

// runStandingJSON emits the structured standing report.
//
// FAC-556: the values come from the run's own Result, NOT from re-parsing the
// prose this command prints. Re-scraping our own output would reproduce the
// exact fragility the consumer reported, one layer down.
func runStandingJSON(cfg *config.Config, mode standing.Mode, only []string, quiet, shutdownDry bool) error {
	res, runErr := standingRunFor(cfg, mode, only, quiet, shutdownDry)
	report := standingJSONReport{Result: res}
	if mode == standing.ModeStatus {
		b := readBrokerHealth(".", cfg)
		report.Broker = &brokerJSON{Serving: b.Serving, Socket: b.Socket, Detail: b.Detail}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return err
	}
	// The run's own error still decides the exit status: a JSON consumer must
	// not read success from a zero exit that only means "encoding worked".
	return runErr
}

// worktreeHeadFor reports a lane's branch and HEAD from its own worktree.
//
// Returns an error rather than empty strings when it cannot tell, so the caller
// omits the fields instead of emitting a plausible zero (FAC-556).
func worktreeHeadFor(cwd string) (string, string, error) {
	branch, err := currentBranch(cwd)
	if err != nil {
		return "", "", err
	}
	head, err := gitLine(cwd, "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	return branch, head, nil
}

// currentBranch reports a directory's checked-out branch.
//
// FAC-556: this exact invocation was written in six places in cmd/herd, and the
// duplicate-rule gate refused a seventh. One definition, so a caller cannot
// disagree with another about what "the branch here" means — the same class of
// divergence that caused FAC-561.
func currentBranch(dir string) (string, error) {
	return gitLine(dir, "rev-parse", "--abbrev-ref", "HEAD")
}

func gitLine(dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
