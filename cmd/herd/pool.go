package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/config"
	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/mergeadmit"
	"github.com/Kampe/Herdforge/pkg/reviewingest"
	"github.com/Kampe/Herdforge/pkg/toolchild"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

// runPool is the small operational surface for the durable warm pool. The
// coordinator owns lease IDs; workers only receive the leased path.
func runPool() {
	fs := flag.NewFlagSet("pool", flag.ContinueOnError)
	root := firstEnv("HERD_ROOT", "HERD_REPO_ROOT", ".")
	size := fs.Int("size", 2, "number of warm worktrees")
	// FAC-577: this flag is the POOL DIRECTORY, but it was named --root, which
	// reads as "repository root". A caller passing `--root .` pointed the pool
	// at the working directory instead of ./.herd/pool, so `release <lease>`
	// answered "lease not found" for a lease that was plainly held, and `list`
	// printed nothing while pool.json held two slots. "Not found" reads as
	// "already released", which is how a consumer concluded a warm slot was free
	// while it was still leased.
	//
	// --pool-root is the documented name and matches `review --pool`'s flag.
	// --root stays as an alias so existing call sites keep working; it has
	// always meant the pool directory, and silently changing its meaning would
	// break anyone who passed the right thing.
	poolDefault := filepath.Join(root, ".herd", "pool")
	poolRoot := fs.String("pool-root", poolDefault, "pool DIRECTORY (not the repository root)")
	poolRootAlias := fs.String("root", "", "alias for --pool-root (pool directory, not the repository root)")
	applyRecovery := fs.Bool("apply", false, "apply an exact recovery manifest (default is validation-only)")
	if err := fs.Parse(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "herd pool: %v\n", err)
		os.Exit(2)
	}
	if fs.NArg() < 1 || fs.NArg() > 2 {
		fmt.Fprintln(os.Stderr, "usage: herd pool [--size N] [--pool-root DIR] [--apply] <ensure|lease|release|gc|list|recover> [argument]")
		os.Exit(2)
	}
	resolvedPool := *poolRoot
	if strings.TrimSpace(*poolRootAlias) != "" {
		resolvedPool = *poolRootAlias
	}
	// A directory with no pool state that CONTAINS a pool is almost certainly a
	// repository root passed by mistake. Say that, instead of letting every
	// subsequent lookup answer "not found" — which reads as "already released".
	if hint, misdirected := misdirectedPoolRoot(resolvedPool); misdirected {
		fmt.Fprintf(os.Stderr,
			"herd pool: %q holds no pool state, but %q does — --pool-root takes the POOL DIRECTORY, not the repository root\n",
			resolvedPool, hint)
		os.Exit(2)
	}
	p := worktree.NewPool(root, resolvedPool, *size)
	ctx := context.Background()
	switch fs.Arg(0) {
	case "ensure":
		if err := p.Ensure(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "herd pool ensure: %v\n", err)
			os.Exit(1)
		}
	case "lease":
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "herd pool lease: purpose is required")
			os.Exit(2)
		}
		lease, err := p.Lease(ctx, fs.Arg(1))
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd pool lease: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("%s\t%s\t%s\n", lease.LeaseID, lease.Path, lease.Purpose)
	case "release":
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "herd pool release: lease id is required")
			os.Exit(2)
		}
		if err := p.Release(ctx, fs.Arg(1)); err != nil {
			fmt.Fprintf(os.Stderr, "herd pool release: %v\n", err)
			os.Exit(1)
		}
	case "gc":
		if err := p.GC(ctx); err != nil {
			fmt.Fprintf(os.Stderr, "herd pool gc: %v\n", err)
			os.Exit(1)
		}
	case "list":
		slots, err := p.Slots()
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd pool list: %v\n", err)
			os.Exit(1)
		}
		for _, slot := range slots {
			fmt.Printf("%s\t%s\t%s\n", slot.Name, slot.Path, slot.Purpose)
		}
	case "recover":
		if fs.NArg() != 2 {
			fmt.Fprintln(os.Stderr, "herd pool recover: exact manifest path is required")
			os.Exit(2)
		}
		recoveryCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		result, err := recoverPoolFromManifest(recoveryCtx, root, p, fs.Arg(1), *applyRecovery)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd pool recover: %v\n", err)
			os.Exit(1)
		}
		out, err := json.Marshal(result)
		if err != nil {
			fmt.Fprintf(os.Stderr, "herd pool recover: encode result: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(string(out))
	default:
		fmt.Fprintf(os.Stderr, "herd pool: unknown action %s\n", fs.Arg(0))
		os.Exit(2)
	}
}

func recoverPoolFromManifest(ctx context.Context, root string, pool *worktree.Pool, manifestPath string, apply bool) (*worktree.ReviewPoolRecoveryResult, error) {
	b, err := os.ReadFile(manifestPath) // #nosec G304 -- explicit operator-selected recovery manifest.
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	req, err := decodePoolRecoveryManifest(b)
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadConfig(filepath.Join(root, ".herd", "herd.yaml"))
	if err != nil {
		return nil, fmt.Errorf("load live project config: %w", err)
	}
	probes := worktree.ReviewPoolRecoveryProbes{
		Hostname: func(context.Context) (string, error) { return os.Hostname() },
		Repository: func(context.Context) (string, error) {
			return toolchild.RepositoryIdentity(root)
		},
		ProjectID: func(context.Context) (string, error) {
			if strings.TrimSpace(cfg.TaskProvider.ProjectID) == "" {
				return "", fmt.Errorf("task_provider.project_id is empty")
			}
			return cfg.TaskProvider.ProjectID, nil
		},
		HolderLive: recoveryHolderLive,
		OpenFiles:  recoveryOpenFiles,
		TaskEvidence: func(ctx context.Context, ref, taskID string) (string, string, error) {
			tp, err := loadTaskProvider(cfg)
			if err != nil {
				return "", "", err
			}
			task, err := tp.GetTask(ctx, taskID)
			if err != nil {
				return "", "", err
			}
			if task == nil || !strings.EqualFold(strings.TrimSpace(task.Ref), strings.TrimSpace(ref)) {
				return "", "", fmt.Errorf("task identity mismatch")
			}
			return mergeadmit.TaskContentRevision(ref, task.Title, task.Description), task.Status, nil
		},
		VerdictEvidence: func(ctx context.Context, path string) (worktree.ReviewPoolVerdictObservation, error) {
			b, err := os.ReadFile(path) // #nosec G304 -- exact repo-contained manifest path is validated by pkg/worktree.
			if err != nil {
				return worktree.ReviewPoolVerdictObservation{}, err
			}
			artifact := reviewingest.Parse(string(b))
			if err := artifact.Validate(nil, func(sha string) bool {
				return exec.CommandContext(ctx, "git", "-C", root, "cat-file", "-e", sha+"^{commit}").Run() == nil
			}); err != nil {
				return worktree.ReviewPoolVerdictObservation{}, err
			}
			return worktree.ReviewPoolVerdictObservation{
				TaskRef: artifact.TaskRef, CandidateSHA: artifact.SHA, Verdict: artifact.Verdict,
				Reviewer: artifact.Reviewer, ReviewerFamily: artifact.ReviewerFamily,
				BuilderFamily: artifact.BuilderFamily, State: filepath.Base(filepath.Dir(path)),
			}, nil
		},
	}
	return pool.RecoverExact(ctx, req, probes, apply)
}

func decodePoolRecoveryManifest(b []byte) (worktree.ReviewPoolRecoveryRequest, error) {
	var req worktree.ReviewPoolRecoveryRequest
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("decode manifest: %w", err)
	}
	var trailing any
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return req, fmt.Errorf("decode manifest: trailing JSON value")
		}
		return req, fmt.Errorf("decode manifest trailer: %w", err)
	}
	return req, nil
}

func recoveryHolderLive(ctx context.Context, purpose string) (bool, error) {
	agents, err := recoveryAgentList(ctx)
	if err != nil {
		return false, err
	}
	want := strings.TrimSpace(purpose)
	for _, agent := range agents {
		if strings.TrimSpace(agent.Name) == want {
			return !settledAgentStatuses[strings.ToLower(strings.TrimSpace(agent.Status))], nil
		}
	}
	return false, nil
}

func recoveryOpenFiles(ctx context.Context, path string) ([]string, error) {
	// Herdr cwd is authoritative for harnesses even when lsof races process
	// startup. Both surfaces must be readable and empty.
	agents, err := recoveryAgentList(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Herdr agents: %w", err)
	}
	var holders []string
	for _, agent := range agents {
		if samePoolRecoveryPath(agent.Cwd, path) || poolRecoveryPathWithin(path, agent.Cwd) {
			holders = append(holders, "herdr:"+agent.Name)
		}
	}
	lsof, err := exec.LookPath("lsof")
	if err != nil {
		return nil, fmt.Errorf("lsof unavailable: %w", err)
	}
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(bounded, lsof, "+D", path)
	out, cmdErr := cmd.CombinedOutput()
	if bounded.Err() != nil {
		return nil, fmt.Errorf("lsof deadline: %w", bounded.Err())
	}
	if cmdErr != nil {
		if exit, ok := cmdErr.(*exec.ExitError); !ok || exit.ExitCode() != 1 || len(bytes.TrimSpace(out)) != 0 {
			return nil, fmt.Errorf("lsof probe: %v (%s)", cmdErr, strings.TrimSpace(string(out)))
		}
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) > 1 {
		holders = append(holders, lines[1:]...)
	}
	return holders, nil
}

func recoveryAgentList(ctx context.Context) ([]herdr.AgentEntry, error) {
	type result struct {
		agents []herdr.AgentEntry
		err    error
	}
	done := make(chan result, 1)
	go func() {
		agents, err := herdr.AgentList()
		done <- result{agents: agents, err: err}
	}()
	select {
	case <-ctx.Done():
		return nil, fmt.Errorf("Herdr agent-list deadline: %w", ctx.Err())
	case got := <-done:
		return got.agents, got.err
	}
}

func samePoolRecoveryPath(a, b string) bool {
	aa, aerr := filepath.Abs(strings.TrimSpace(a))
	bb, berr := filepath.Abs(strings.TrimSpace(b))
	return aerr == nil && berr == nil && filepath.Clean(aa) == filepath.Clean(bb)
}

func poolRecoveryPathWithin(parent, child string) bool {
	if strings.TrimSpace(parent) == "" || strings.TrimSpace(child) == "" {
		return false
	}
	pa, perr := filepath.Abs(parent)
	ca, cerr := filepath.Abs(child)
	if perr != nil || cerr != nil {
		return false
	}
	rel, err := filepath.Rel(pa, ca)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// misdirectedPoolRoot reports whether a pool root looks like a repository root.
//
// FAC-577: the signal is unambiguous — the given directory has no pool.json but
// <dir>/.herd/pool does. Guessing is not involved: there is a pool, and it is
// not where we were told to look.
func misdirectedPoolRoot(dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", false
	}
	nested := filepath.Join(dir, ".herd", "pool")
	_, hereErr := os.Stat(filepath.Join(dir, "pool.json"))
	_, nestedErr := os.Stat(filepath.Join(nested, "pool.json"))
	if nestedErr != nil {
		// No pool underneath, so nothing suggests a misdirection.
		return "", false
	}
	if hereErr != nil {
		return nested, true
	}
	// BOTH exist. The one here is almost certainly stray: a previous misuse of
	// this flag creates an empty pool.json wherever it was pointed, and that
	// file then makes the mistake look legitimate forever. Reporting it is the
	// only way the operator learns which of the two is real — I created exactly
	// such a file in this repository by making this mistake myself.
	return nested, true
}
