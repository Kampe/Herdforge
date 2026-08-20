package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/boardfreeze"
	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/eligibility"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/lifecycle"
	"github.com/Kampe/Herdforge/pkg/resources"
	"github.com/Kampe/Herdforge/pkg/standing"
	"github.com/Kampe/Herdforge/pkg/usage"
	"github.com/Kampe/Herdforge/pkg/wave"
)

// runWave is the FAC-105 operator entry point for a controlled work wave.
// Default is a read-only pre-wave report. --standing / --up raise only
// configured standing roles after every readiness gate passes. Wave never
// claims board work; claimable refs are reported as next-action handoffs.
//
// Standing raise reuses pkg/standing via runStandingConfigMode (FAC-91) — it
// does not reimplement tab create/start.
func runWave() {
	fs := flag.NewFlagSet("wave", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	doStanding := fs.Bool("standing", false, "Raise configured standing roles after every readiness gate passes")
	up := fs.Bool("up", false, "Alias of --standing: raise standing roles after gates pass")
	asJSON := fs.Bool("json", false, "Emit the stable wave report as JSON")
	if err := fs.Parse(os.Args[2:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(usageFor("wave"))
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "herd wave: %v\n", err)
		os.Exit(2)
	}

	opts := wave.Options{Standing: *doStanding, Up: *up}
	ctx := context.Background()

	cfg, cfgErr := config.LoadConfig(".herd/herd.yaml")
	src, raiser := buildWaveRuntime(ctx, cfg, cfgErr)

	rep, err := wave.Run(ctx, src, opts, raiser)
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if encErr := enc.Encode(rep); encErr != nil {
			fmt.Fprintf(os.Stderr, "herd wave: json encode: %v\n", encErr)
			os.Exit(1)
		}
	} else if rep != nil {
		fmt.Print(wave.FormatHuman(rep))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "herd wave: %v\n", err)
		os.Exit(1)
	}
	if rep != nil && !rep.Ready && opts.WantsRaise() {
		os.Exit(1)
	}
}

// buildWaveRuntime wires production read-only sources and an optional raiser.
// cfg may be nil when load failed; standing_roster then fails closed.
func buildWaveRuntime(ctx context.Context, cfg *config.Config, cfgErr error) (wave.Sources, wave.Raiser) {
	src := wave.Sources{
		ReviewCap: 3,
		// Same production posture gate as herd standing / pulse (cmd requireFleetAdmission).
		Winddown: requireFleetAdmission,
		BoardFreeze: func() (bool, string, error) {
			st, frozen, err := boardfreeze.Active(time.Now())
			if err != nil {
				return true, "", err
			}
			detail := fmt.Sprintf("on=%v generation=%d actor=%s", st.On, st.Generation, st.Actor)
			return frozen, detail, nil
		},
		Resources: func() (string, string) {
			s := resources.TakeSnapshot()
			return s.Verdict, fmt.Sprintf("verdict=%s free_pct=%d swap_mb=%d", s.Verdict, s.FreePct, s.SwapMB)
		},
		Quota: func() (bool, string, error) {
			snap, err := usage.FetchSnapshot()
			if err != nil {
				return false, "", fmt.Errorf("live quota unreadable: %w", err)
			}
			if snap == nil || len(snap.Providers) == 0 {
				return true, "quota snapshot empty (no providers reported)", nil
			}
			return true, fmt.Sprintf("quota snapshot ok (%d provider(s))", len(snap.Providers)), nil
		},
		HerdrOK: func() (bool, string) {
			if !herdr.IsAvailable() {
				return false, "herdr CLI not found"
			}
			return true, "herdr available"
		},
	}

	if cfgErr != nil || cfg == nil {
		src.StandingLanes = func() []wave.Lane { return nil }
	} else {
		cfg := cfg
		src.StandingLanes = func() []wave.Lane {
			lanes := standing.StandingLanes(cfg)
			out := make([]wave.Lane, 0, len(lanes))
			for _, lane := range lanes {
				out = append(out, wave.Lane{
					Name:      lane.Name,
					AgentName: standing.AgentNameForRepository(lane.Name, repositoryIdentityForLaunch(cfg)),
				})
			}
			return out
		}
	}

	src.LiveAgents = func() ([]wave.Agent, error) {
		agents, err := herdr.AgentList()
		if err != nil {
			return nil, err
		}
		out := make([]wave.Agent, 0, len(agents))
		for _, a := range agents {
			if strings.TrimSpace(a.Name) == "" {
				continue
			}
			out = append(out, wave.Agent{
				Name:   a.Name,
				Status: a.Status,
				PaneID: a.PaneID,
				TabID:  a.TabID,
			})
		}
		return out, nil
	}

	// Held: durable hold authority. Any authority/registry failure fails closed
	// by treating the lane as held so raise skips rather than launching into
	// unknown hold state.
	src.Held = buildWaveHeldChecker(ctx, cfg, cfgErr)

	// Claimable + in-review: read-only provider list. Provider failure is a
	// non-blocking unknown enrichment (raise is fleet posture, not claim).
	if cfg != nil && cfgErr == nil {
		cfg := cfg
		src.Claimable = func(c context.Context) ([]wave.ClaimableRef, error) {
			tp, err := loadTaskProvider(cfg)
			if err != nil {
				return nil, err
			}
			// Empty claimRole = hygiene scan (no role filter). Empty Facts =
			// no external blocker/dupe evidence; dispatch still re-gates.
			rep, err := eligibility.EvaluateBoard(c, tp, cfg.TaskProvider.ProjectID, "to-do", eligibility.Facts{}, "")
			if err != nil {
				return nil, err
			}
			out := make([]wave.ClaimableRef, 0, len(rep.Eligible))
			for _, r := range rep.Eligible {
				out = append(out, wave.ClaimableRef{
					Ref:      r.Ref,
					Title:    r.Title,
					Priority: string(r.Priority),
					Role:     r.Role,
				})
			}
			return out, nil
		}
		src.InReview = func(c context.Context) (int, error) {
			tp, err := loadTaskProvider(cfg)
			if err != nil {
				return 0, err
			}
			tasks, err := tp.ListTasks(c, cfg.TaskProvider.ProjectID, "in-review")
			if err != nil {
				return 0, err
			}
			return len(tasks), nil
		}
	}

	var raiser wave.Raiser
	if cfg != nil && cfgErr == nil {
		raiser = &standingRaiser{cfg: cfg}
	}
	return src, raiser
}

func buildWaveHeldChecker(ctx context.Context, cfg *config.Config, cfgErr error) func(string) (bool, string) {
	if cfgErr != nil || cfg == nil {
		return func(string) (bool, string) {
			return true, "hold check unavailable: config load failed"
		}
	}
	holdAuth, holdErr := newProductionHoldAuthority()
	if holdErr != nil {
		return func(string) (bool, string) {
			return true, "hold authority unavailable: " + holdErr.Error()
		}
	}
	repoIdentity, repoErr := holdRepository()
	if repoErr != nil {
		_ = holdAuth.Close()
		return func(string) (bool, string) {
			return true, "repository identity unavailable: " + repoErr.Error()
		}
	}
	registry, regErr := canonicalLaneRegistry(cfg)
	if regErr != nil {
		_ = holdAuth.Close()
		return func(string) (bool, string) {
			return true, "lane registry unavailable: " + regErr.Error()
		}
	}
	activeResolver, resErr := loadProductionActiveTaskResolver(ctx)
	if resErr != nil {
		// Fail closed on resolver errors: cannot know task-scope holds.
		_ = holdAuth.Close()
		return func(string) (bool, string) {
			return true, "active task resolver unavailable: " + resErr.Error()
		}
	}
	// holdAuth intentionally kept open for the process lifetime of this CLI
	// invocation (same pattern as herd attention).
	return func(agentName string) (bool, string) {
		lane, err := registry.ResolveLiveAgentID(agentName)
		if err != nil {
			return true, "resolve lane: " + err.Error()
		}
		generation := func(c context.Context, identity lifecycle.HoldIdentity) (int64, error) {
			return holdAuth.CurrentGeneration(c, identity)
		}
		err = lifecycle.CheckLaneAndTaskHold(ctx, holdAuth, activeResolver, repoIdentity, lane.Role, lane.Name, generation)
		if err == nil {
			return false, ""
		}
		if errors.Is(err, lifecycle.ErrHoldDenied) {
			return true, err.Error()
		}
		return true, "hold check failed: " + err.Error()
	}
}

// standingRaiser raises one configured standing lane through the FAC-91
// production standing path (runStandingConfigMode). Plan already skipped
// live/held agents; standing.Run re-checks NameHeld before create so a
// concurrent raise cannot duplicate a standing agent.
type standingRaiser struct {
	cfg *config.Config
}

func (r *standingRaiser) Raise(ctx context.Context, lane wave.Lane) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if r.cfg == nil {
		return errors.New("wave raise: config required")
	}
	if !herdr.IsAvailable() {
		return errors.New("herdr CLI not found — install herdr first")
	}
	// quiet=true: wave already prints its own raise_results JSON/human report.
	return runStandingConfigMode(r.cfg, true, standing.ModeRaise, []string{lane.Name}, true, false)
}
