package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/shot"
)

// runShot ports bin/herd-shot: one bounded task, headless, through the quota
// router. No tab, no pane, no session bootstrap.
func runShot() {
	fs := flag.NewFlagSet("shot", flag.ExitOnError)
	shape := fs.String("task", shot.DefaultShape, "Task shape (bounded, research, qa, ...)")
	provider := fs.String("provider", "", "Pin a surface (must be able to do the job)")
	schema := fs.String("schema", "", "JSON Schema file constraining the output")
	timeoutSec := fs.Int("timeout", 900, "Seconds before the shot is killed")
	dryRun := fs.Bool("dry-run", false, "Print the routed decision without executing")
	fs.Parse(os.Args[2:])

	prompt := strings.Join(fs.Args(), " ")
	if prompt == "-" {
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd shot: read stdin: %v\n", err)
			os.Exit(2)
		}
		prompt = string(body)
	}
	if strings.TrimSpace(prompt) == "" {
		fmt.Fprintln(os.Stderr, "herd shot: prompt required (argument or '-' for stdin)")
		os.Exit(2)
	}
	if *timeoutSec <= 0 {
		fmt.Fprintln(os.Stderr, "herd shot: --timeout must be a positive integer")
		os.Exit(2)
	}
	if *schema != "" {
		if _, err := os.Stat(*schema); err != nil {
			fmt.Fprintf(os.Stderr, "herd shot: --schema file %s not found\n", *schema)
			os.Exit(2)
		}
	}

	req := shot.Request{Shape: *shape, Provider: *provider, Schema: *schema, Prompt: prompt}
	if err := req.ValidatePin(); err != nil {
		fmt.Fprintf(os.Stderr, "herd shot: %v\n", err)
		os.Exit(2)
	}

	tried := map[string]bool{}
	// Reroute exactly ONCE. A thin or garbage result is not evidence of a dead
	// provider, so it must never cool a surface and must not loop the fleet
	// through every candidate hunting for an answer it likes.
	for attempt := 0; attempt < 2; attempt++ {
		decision, err := routeShot(req, tried)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd shot: %v\n", err)
			os.Exit(1)
		}
		tried[decision.Provider] = true

		if *dryRun {
			fmt.Printf("herd shot: would run %s/%s effort=%s shape=%s (%s)\n",
				decision.Provider, decision.Model, decision.Effort, decision.Shape, decision.Availability)
			return
		}

		body, runErr := execShot(decision, prompt, time.Duration(*timeoutSec)*time.Second)
		if runErr != nil {
			fmt.Fprintf(os.Stderr, "herd shot: %s/%s failed: %v\n", decision.Provider, decision.Model, runErr)
			if attempt == 0 {
				continue
			}
			os.Exit(1)
		}
		if err := req.ValidateOutput(body); err != nil {
			fmt.Fprintf(os.Stderr, "herd shot: %s/%s produced an unusable result: %v\n",
				decision.Provider, decision.Model, err)
			if attempt == 0 {
				fmt.Fprintln(os.Stderr, "herd shot: rerouting once to the next eligible surface")
				continue
			}
			os.Exit(1)
		}
		fmt.Print(body)
		if !strings.HasSuffix(body, "\n") {
			fmt.Println()
		}
		return
	}
}

// routeShot picks a surface, excluding those that structurally cannot do the
// job plus any already attempted.
func routeShot(req shot.Request, tried map[string]bool) (*router.LaunchDecision, error) {
	if req.Provider != "" && !tried[req.Provider] {
		// An explicit pin is honoured once; it was already validated as capable.
		if m := router.ModelFor(req.Provider, req.Shape); !router.AuthoringModelAllowed(m) {
			return nil, fmt.Errorf("%s/%s is coordinator-only and may not be used for a shot", req.Provider, m)
		}
		return &router.LaunchDecision{
			Provider: req.Provider,
			Model:    "",
			Effort:   router.EffortFor(req.Shape),
			Shape:    req.Shape,
		}, nil
	}
	r := router.NewRouter(nil, nil)
	for _, p := range req.Eligible() {
		if tried[p] {
			continue
		}
		route, err := r.Pick(req.Shape, p, "")
		if err != nil {
			continue
		}
		// routeShot uses Pick, not Decide, so the coordinator-only guard inside
		// Decide never ran here. A coordinator- or architecture-shaped shot can
		// resolve to a fable-flavoured model on claude or lazer.
		if !router.AuthoringModelAllowed(route.Model) {
			continue
		}
		return &router.LaunchDecision{
			Provider: route.Provider, Model: route.Model,
			Effort: route.Effort, Shape: req.Shape,
			Availability: route.Availability,
		}, nil
	}
	return nil, fmt.Errorf("no eligible surface left for a %q shot (tried %d)", req.Shape, len(tried))
}

// execShot runs the routed surface headlessly with a hard timeout.
func execShot(d *router.LaunchDecision, prompt string, timeout time.Duration) (string, error) {
	argv := router.ArgvFor(d.Provider, d.Model, d.Effort)
	if len(argv) == 0 {
		return "", fmt.Errorf("no launch argv contract for provider %q", d.Provider)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Stdin = strings.NewReader(prompt)
	out, err := cmd.Output()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("timed out after %s", timeout)
	}
	if err != nil {
		return "", err
	}
	return string(out), nil
}
