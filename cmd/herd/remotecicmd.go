package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/recovery"
	"github.com/Kampe/Herdforge/pkg/remoteci"
	hsync "github.com/Kampe/Herdforge/pkg/sync"
	"github.com/Kampe/Herdforge/pkg/toolchild"
)

const (
	defaultRemoteCITimeout      = 15 * time.Minute
	maximumRemoteCITimeout      = 30 * time.Minute
	defaultRemoteCIPollInterval = 10 * time.Second
	defaultRemoteCIMaxPolls     = 90
)

type remoteCISettleOutput struct {
	Version        int                  `json:"version"`
	Ref            string               `json:"ref"`
	Repository     string               `json:"repository"`
	CandidateSHA   string               `json:"candidate_sha"`
	PolicyRevision string               `json:"policy_revision"`
	RequiredChecks []string             `json:"required_checks"`
	Attempt        int64                `json:"attempt"`
	State          remoteci.State       `json:"state,omitempty"`
	Observation    remoteci.Observation `json:"observation,omitempty"`
	Registered     bool                 `json:"registered"`
	Polls          int                  `json:"polls"`
	Ledger         string               `json:"ledger"`
	RemoteCIArgs   []string             `json:"remote_ci_args,omitempty"`
	Error          string               `json:"error,omitempty"`
}

func runRemoteCISettle() {
	result, err := settleRemoteCI(context.Background(), os.Args[2:])
	if err != nil {
		result.Error = remoteci.SanitizeDiagnostic(err.Error())
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if encodeErr := encoder.Encode(result); encodeErr != nil {
		fmt.Fprintf(os.Stderr, "herd remote-ci-settle: encode result: %v\n", encodeErr)
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
}

func settleRemoteCI(ctx context.Context, args []string) (remoteCISettleOutput, error) {
	fs := flag.NewFlagSet("remote-ci-settle", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	ref := fs.String("ref", "", "Task reference used for terminal failure routing (required)")
	candidate := fs.String("candidate", "", "Exact lowercase candidate SHA (required)")
	attempt := fs.Int64("remote-ci-attempt", 0, "Exact positive GitHub Actions attempt (required)")
	ledger := fs.String("remote-ci-file", remoteci.DefaultLedgerPath, "Durable remote-CI settlement ledger")
	timeout := fs.Duration("timeout", defaultRemoteCITimeout, "Hard polling timeout")
	interval := fs.Duration("poll-interval", defaultRemoteCIPollInterval, "Delay between transient provider observations")
	maxPolls := fs.Int("max-polls", defaultRemoteCIMaxPolls, "Hard provider observation limit")
	if err := fs.Parse(args); err != nil {
		return remoteCISettleOutput{}, fmt.Errorf("remote-ci-settle arguments: %w", err)
	}
	if fs.NArg() != 0 {
		return remoteCISettleOutput{}, fmt.Errorf("remote-ci-settle accepts flags only")
	}
	refValue := hsync.NormalizeRef(strings.TrimSpace(*ref))
	if refValue == "" {
		return remoteCISettleOutput{}, fmt.Errorf("remote-ci-settle requires --ref")
	}
	if *timeout <= 0 || *timeout > maximumRemoteCITimeout {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle --timeout must be positive and at most %s", maximumRemoteCITimeout)
	}
	if *interval <= 0 || *interval > *timeout {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle --poll-interval must be positive and no greater than --timeout")
	}
	if *maxPolls < 1 || *maxPolls > 10000 {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle --max-polls must be between 1 and 10000")
	}

	policy, err := preflight.LoadMergePolicy(".")
	if err != nil {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle live policy: %w", err)
	}
	policyReport := preflight.CheckMergePolicy(policy)
	if !policyReport.OK {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle live policy refused: %s", strings.Join(policyReport.Reasons, "; "))
	}
	if !policy.RemoteCI.Required {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle refused: live merge policy does not require remote CI")
	}
	repository, err := toolchild.RepositoryIdentity(".")
	if err != nil {
		return remoteCISettleOutput{Ref: refValue}, fmt.Errorf("remote-ci-settle repository identity: %w", err)
	}
	binding, err := remoteci.NewBinding(repository, *candidate, preflight.PolicyRevision(policy), *attempt, policy.RemoteCI.RequiredChecks)
	if err != nil {
		return remoteCISettleOutput{Ref: refValue, Repository: repository, CandidateSHA: *candidate, Attempt: *attempt}, fmt.Errorf("remote-ci-settle binding: %w", err)
	}
	output := remoteCISettleOutput{
		Version: 1, Ref: refValue, Repository: binding.Repository,
		CandidateSHA: binding.CandidateSHA, PolicyRevision: binding.PolicyRevision,
		RequiredChecks: append([]string(nil), binding.RequiredChecks...), Attempt: binding.Attempt,
		Ledger: *ledger,
	}
	store, err := remoteci.Open(*ledger)
	if err != nil {
		return output, err
	}
	pollCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	poller := remoteci.Poller{
		Watcher: remoteci.GitHubActions{Execute: executeRemoteCIGH},
		Store:   store,
		Router: remoteCIFailureRouter{
			Path: filepath.Join(filepath.Dir(*ledger), "recovery.json"),
			Ref:  refValue,
		},
		PollInterval: *interval,
		MaxPolls:     *maxPolls,
	}
	pollResult, pollErr := poller.Run(pollCtx, binding)
	output.Registered = pollResult.Registered
	output.Polls = pollResult.Polls
	output.Observation = pollResult.Observation
	output.State = pollResult.Settlement.State
	if pollErr != nil {
		return output, pollErr
	}
	output.RemoteCIArgs = []string{
		"--remote-ci-attempt", strconv.FormatInt(binding.Attempt, 10),
		"--remote-ci-file", *ledger,
	}
	return output, nil
}

func executeRemoteCIGH(ctx context.Context, name string, args ...string) ([]byte, error) {
	if name != "gh" {
		return nil, fmt.Errorf("remote-ci: refused unexpected provider executable %q", name)
	}
	return exec.CommandContext(ctx, name, args...).Output()
}

type remoteCIFailureRouter struct {
	Path string
	Ref  string
}

func (r remoteCIFailureRouter) RouteTerminalFailure(ctx context.Context, settlement remoteci.Settlement) error {
	store, err := recovery.Open(r.Path)
	if err != nil {
		return err
	}
	router := remoteci.RecoveryRouter{
		Store: store, Run: "remote-ci:" + settlement.Binding.CandidateSHA,
		Task: r.Ref, Actor: "remote-ci-settle", Revision: settlement.Binding.Attempt,
		Graph: settlement.Binding.PolicyRevision,
	}
	return router.RouteTerminalFailure(ctx, settlement)
}
