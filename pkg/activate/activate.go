// Package activate brings up the shared runtime deployables and verifies
// health via a dual compose-state + /v1/status gate.
//
// Port of bin/herd-activate (zsh, 212 lines). The shell script's central
// lesson — encoded here as invariants — is that a coordinator must NEVER
// type a partial `docker compose up -d api worker` list that silently
// drops the omitted services (the root cause of recurring indexer-evm
// drops), and that compose state must be verified IN ADDITION to
// /v1/status because deployable health is heartbeat-ledger based with a
// 3-minute stale window: a container stuck in Created/Exited can still
// look ok on /v1/status until heartbeats age out.
//
// Run applies migrations (asserted, before boot), brings up ALL
// deployables detached, then polls for up to Timeout: every poll checks
// BOTH `docker compose ps` state AND /v1/status. It returns healthy only
// when every required deployable is compose-running AND /v1/status
// reports overall=ok with no unhealthy deployables. On health it kicks
// the standing fleet (suppressed by NoFleet / HERD_WIND_DOWN).
package activate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// DefaultDeployables is the canonical five-deployable runtime set.
// A port that dropped a service would reintroduce the indexer-evm drops.
var DefaultDeployables = []string{
	"api", "core", "indexer-evm", "indexer-other", "worker",
}

const (
	DefaultAPIURL       = "http://localhost:13100"
	DefaultWebURL       = "http://localhost:4174"
	DefaultTimeout      = 60 * time.Second
	DefaultPollInterval = 5 * time.Second
)

// StatusUnhealthy inspects a /v1/status JSON body and returns the
// comma-joined names of deployables whose status != "ok". Returns "" when
// all deployables are ok or the JSON is unparseable/empty (defensive: a
// missing data.deployables array yields "" rather than panicking).
//
// Mirrors bin/herd-activate status_unhealthy().
func StatusUnhealthy(statusJSON string) string {
	if strings.TrimSpace(statusJSON) == "" {
		return ""
	}
	var doc struct {
		Data struct {
			Status      string `json:"status"`
			Deployables []struct {
				Name   string `json:"name"`
				Status string `json:"status"`
			} `json:"deployables"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(statusJSON), &doc); err != nil {
		return ""
	}
	var bad []string
	for _, d := range doc.Data.Deployables {
		if d.Status != "ok" {
			bad = append(bad, d.Name)
		}
	}
	return strings.Join(bad, ",")
}

// overallFromStatus extracts .data.status from a /v1/status body.
// Returns "" when absent or unparseable.
func overallFromStatus(statusJSON string) string {
	if strings.TrimSpace(statusJSON) == "" {
		return ""
	}
	var doc struct {
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(statusJSON), &doc); err != nil {
		return ""
	}
	return doc.Data.Status
}

// ComposeNotRunning inspects `docker compose ps --format '{{.Service}}
// {{.State}}'` output and returns comma-joined "svc:state" entries for
// any required service that is missing or not in "running" state.
// Returns "" when all required services are running.
//
// Blank lines are ignored. A service absent from the ps output is
// reported as "svc:missing". Mirrors bin/herd-activate
// compose_not_running().
func ComposeNotRunning(psText string, required []string) string {
	stateBy := make(map[string]string, len(required))
	for _, line := range strings.Split(psText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// "service state ..." — take first two fields.
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		stateBy[fields[0]] = fields[1]
	}
	var bad []string
	for _, svc := range required {
		st := stateBy[svc]
		if st == "" {
			st = "missing"
		}
		if st != "running" {
			bad = append(bad, svc+":"+st)
		}
	}
	return strings.Join(bad, ",")
}

// Selftest exercises the pure predicates against the same fixture shapes
// used by `bin/herd-activate --selftest`. Returns nil on pass, error on
// fail. A regression in either predicate (e.g. treating "created" as
// running, or dropping the missing-service case) is caught here.
func Selftest() error {
	ok := `{"data":{"status":"ok","deployables":[{"name":"api","status":"ok"},{"name":"indexer-evm","status":"ok"}]}}`
	deg := `{"data":{"status":"degraded","deployables":[{"name":"api","status":"ok"},{"name":"indexer-evm","status":"down"}]}}`
	if StatusUnhealthy(ok) != "" || StatusUnhealthy(deg) != "indexer-evm" {
		return fmt.Errorf("FAIL status predicate")
	}

	sample := "api running\ncore created\nindexer-evm running\nindexer-other running\nworker created\n"
	got := ComposeNotRunning(sample, DefaultDeployables)
	if got != "core:created,worker:created" {
		return fmt.Errorf("FAIL compose predicate got=%s", got)
	}
	sampleOK := "api running\ncore running\nindexer-evm running\nindexer-other running\nworker running\n"
	if ComposeNotRunning(sampleOK, DefaultDeployables) != "" {
		return fmt.Errorf("FAIL compose predicate expected empty")
	}
	got = ComposeNotRunning("api running\n", []string{"api", "core"})
	if got != "core:missing" {
		return fmt.Errorf("FAIL compose missing got=%s", got)
	}
	return nil
}

// --- Orchestrator ---

// ComposeClient abstracts the docker compose operations used by Run so
// the activation flow is testable without a docker daemon.
type ComposeClient interface {
	Build(services []string) error
	Up(detached bool, services []string) error
	Start(services []string) error
	PsFormat(format string, services []string) (string, error)
}

// Migrator abstracts the pre-boot asserted migration step.
type Migrator interface {
	Apply() error
}

// HTTPGetter abstracts HTTP GET used for /v1/status and the web probe.
type HTTPGetter interface {
	Get(url string) (code int, body string, err error)
}

// FleetKicker abstracts the post-activation standing-fleet re-engage.
type FleetKicker interface {
	Kick(reason string) error
}

// Options configures an activation run. Deployables, APIURL, WebURL,
// Timeout, and PollInterval default to the package defaults when zero.
// Compose, Migrator, HTTP, and Fleet default to the real shell-out /
// net/http implementations when nil; tests inject fakes.
type Options struct {
	Deployables   []string
	APIURL        string
	WebURL        string
	BuildServices []string
	NoFleet       bool
	Timeout       time.Duration
	PollInterval  time.Duration
	Compose       ComposeClient
	Migrator      Migrator
	HTTP          HTTPGetter
	Fleet         FleetKicker
}

// Result is the final state of an activation run.
type Result struct {
	Healthy     bool
	Overall     string
	Unhealthy   string
	NotRunning  string
	WebCode     int
	FleetKicked bool
}

// Run executes the activation flow:
//  1. Build requested images (if BuildServices non-empty).
//  2. Apply migrations (asserted, before boot) — refuse to boot on a
//     stale schema rather than serve 500s.
//  3. compose up -d ALL deployables (never a partial list).
//  4. Snapshot compose ps; attempt `compose start` (not recreate) for
//     containers left in Created after a partial activate.
//  5. Poll for up to Timeout: healthy iff compose all running AND
//     /v1/status overall=ok AND no unhealthy deployables. On health,
//     probe the web URL and kick the standing fleet (unless NoFleet).
//
// Returns (Result, nil) on health, (Result, error) on unhealthy-after-
// timeout, and (nil, error) on a hard failure that prevents boot
// (build/migration/up failure).
func Run(opts Options) (*Result, error) {
	if len(opts.Deployables) == 0 {
		opts.Deployables = DefaultDeployables
	}
	if opts.APIURL == "" {
		opts.APIURL = os.Getenv("OV_LOCAL_API_URL")
	}
	if opts.WebURL == "" {
		opts.WebURL = os.Getenv("OV_LOCAL_WEB_URL")
	}
	if opts.APIURL == "" {
		opts.APIURL = DefaultAPIURL
	}
	if opts.WebURL == "" {
		opts.WebURL = DefaultWebURL
	}
	if opts.Timeout == 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.PollInterval == 0 {
		opts.PollInterval = DefaultPollInterval
	}
	if opts.Compose == nil {
		// Real default path: canonical-checkout guard + repo-root pinning
		// must happen BEFORE the first compose process is spawned.
		if _, err := prepareRealEnv(); err != nil {
			return nil, err
		}
		opts.Compose = newDockerCompose()
	}
	if opts.Migrator == nil {
		opts.Migrator = binMigrator{root: resolveRepoRoot()}
	}
	if opts.HTTP == nil {
		opts.HTTP = httpGetter{client: &http.Client{Timeout: 8 * time.Second}}
	}
	if opts.Fleet == nil {
		opts.Fleet = herdKickFleet{root: resolveRepoRoot()}
	}

	if len(opts.BuildServices) > 0 {
		if err := opts.Compose.Build(opts.BuildServices); err != nil {
			return nil, fmt.Errorf("herd-activate: build failed: %w", err)
		}
	}

	if err := opts.Migrator.Apply(); err != nil {
		return nil, fmt.Errorf("herd-activate: migration step failed, refusing to boot deployables onto a stale schema: %w", err)
	}

	if err := opts.Compose.Up(true, opts.Deployables); err != nil {
		return nil, fmt.Errorf("herd-activate: compose up failed: %w", err)
	}

	psText, _ := opts.Compose.PsFormat("{{.Service}} {{.State}}", opts.Deployables)
	if notRunning := ComposeNotRunning(psText, opts.Deployables); notRunning != "" {
		// restart != recreate: start is the only recovery for containers
		// left in Created after a partial activate.
		opts.Compose.Start(opts.Deployables)
	}

	deadline := time.Now().Add(opts.Timeout)
	res := &Result{}
	for time.Now().Before(deadline) {
		psText, _ := opts.Compose.PsFormat("{{.Service}} {{.State}}", opts.Deployables)
		res.NotRunning = ComposeNotRunning(psText, opts.Deployables)

		code, body, err := opts.HTTP.Get(opts.APIURL + "/v1/status")
		if err != nil || code != 200 || strings.TrimSpace(body) == "" {
			res.Overall = "unreachable"
			res.Unhealthy = "api-unreachable"
		} else {
			res.Overall = overallFromStatus(body)
			res.Unhealthy = StatusUnhealthy(body)
		}

		if res.NotRunning == "" && res.Overall == "ok" && res.Unhealthy == "" {
			wcode, _, werr := opts.HTTP.Get(opts.WebURL + "/")
			if werr != nil {
				res.WebCode = 0
			} else {
				res.WebCode = wcode
			}
			res.Healthy = true
			if !opts.NoFleet {
				if err := opts.Fleet.Kick("post-activation main healthy; re-scan/guard/rank"); err == nil {
					res.FleetKicked = true
				}
			}
			return res, nil
		}
		time.Sleep(opts.PollInterval)
	}

	return res, fmt.Errorf("herd-activate: UNHEALTHY after %s — status=%s api_unhealthy=%s compose_not_running=%s",
		opts.Timeout, res.Overall, res.Unhealthy, res.NotRunning)
}

// resolveRepoRoot returns the canonical repo root for repo-relative
// subprocess calls. Resolution order: $HERD_ROOT (explicit), then git
// discovery via `git rev-parse --path-format=absolute --git-common-dir`
// (parent of <root>/.git). Returns "" when no root is determinable and
// does NOT fall back to the raw CWD, so subprocess calls never run
// repo-relative paths from an arbitrary working directory.
func resolveRepoRoot() string {
	if r := os.Getenv("HERD_ROOT"); r != "" {
		return r
	}
	out, err := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(out))
	parent := filepath.Dir(common)
	if parent == "" || parent == "." {
		return ""
	}
	return parent
}

// ensureCanonicalCheckout refuses product compose/activate from a git
// worktree. Port of herd_require_canonical_checkout (herd-lib.zsh): the
// canonical shared checkout is exactly the directory that owns the
// resolved --git-common-dir (i.e. pwd -P == dirname(<root>/.git)). Any
// other location (linked worktree, nested dir) is refused to prevent
// forked bind mounts against an unguarded stack.
func ensureCanonicalCheckout() error {
	out, err := exec.Command("git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return fmt.Errorf("herd-activate: not a git checkout (git rev-parse --git-common-dir failed): %w", err)
	}
	common := strings.TrimSpace(string(out))
	canonP, err := filepath.EvalSymlinks(filepath.Dir(common))
	if err != nil {
		return fmt.Errorf("herd-activate: resolve canonical root from %q: %w", common, err)
	}
	here, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("herd-activate: getwd: %w", err)
	}
	hereP, err := filepath.EvalSymlinks(here)
	if err != nil {
		return fmt.Errorf("herd-activate: resolve cwd %q: %w", here, err)
	}
	if hereP != canonP {
		return fmt.Errorf("herd-activate: canonical shared checkout required (got %s, need %s); refuse product compose/activate from a worktree", hereP, canonP)
	}
	return nil
}

// prepareRealEnv pins a real activation run to the canonical checkout
// before any subprocess spawns: it refuses worktrees, resolves and chdirs
// into the repo root so repo-relative subprocess calls resolve, and
// exports OV_STACK_GUARD=1 before the first compose call. Only invoked on
// the real default path (opts.Compose == nil); tests that inject fakes
// never touch the shared checkout.
func prepareRealEnv() (string, error) {
	if err := ensureCanonicalCheckout(); err != nil {
		return "", err
	}
	root := resolveRepoRoot()
	if root == "" {
		return "", fmt.Errorf("herd-activate: cannot resolve repo root; set HERD_ROOT or run inside a git checkout")
	}
	if err := os.Chdir(root); err != nil {
		return "", fmt.Errorf("herd-activate: chdir %q: %w", root, err)
	}
	os.Setenv("OV_STACK_GUARD", "1")
	return root, nil
}

// --- Real default implementations (shell-out / net/http) ---

// dockerCompose shells out to `docker compose` (preferred) or standalone
// `docker-compose`, auto-detecting the colima socket when DOCKER_HOST is
// unset. Mirrors the shell's COMPOSE transport selection.
//
// product compose refuses without OV_STACK_GUARD=1 (docker-compose.yml
// x-stack-guard); newDockerCompose exports it BEFORE the first compose
// process is spawned, mirroring herd-activate:104.
type dockerCompose struct{ cmd []string }

func newDockerCompose() ComposeClient {
	// HARD BAN: never product-compose from a git worktree (forked bind
	// mounts) — port of herd_require_canonical_checkout (herd-lib.zsh).
	// Serialization under the shared-checkout advisory lock before compose
	// mutation is not ported here — tracked as FAC-87.
	if root := resolveRepoRoot(); root != "" {
		os.Chdir(root)
	}

	// Export the compose stack guard before any compose call. The sanctioned
	// path sets it here so raw `docker compose` still refuses to run.
	os.Setenv("OV_STACK_GUARD", "1")

	if path, err := exec.LookPath("docker"); err == nil {
		_ = path
		return dockerCompose{cmd: []string{"docker", "compose"}}
	}
	if _, err := exec.LookPath("docker-compose"); err == nil {
		if os.Getenv("DOCKER_HOST") == "" {
			if sock := os.Getenv("HOME") + "/.colima/default/docker.sock"; fileExists(sock) {
				os.Setenv("DOCKER_HOST", "unix://"+sock)
			}
		}
		return dockerCompose{cmd: []string{"docker-compose"}}
	}
	// Unreachable in practice — the shell hard-errors. Tests inject
	// fakes, and the subcommand surfaces the error on first use.
	return dockerCompose{cmd: []string{"docker", "compose"}}
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func (d dockerCompose) run(args ...string) error {
	full := append(append([]string{}, d.cmd...), args...)
	c := exec.Command(full[0], full[1:]...)
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func (d dockerCompose) Build(services []string) error {
	return d.run(append([]string{"build"}, services...)...)
}

func (d dockerCompose) Up(detached bool, services []string) error {
	args := []string{"up"}
	if detached {
		args = append(args, "-d")
	}
	return d.run(append(args, services...)...)
}

func (d dockerCompose) Start(services []string) error {
	return d.run(append([]string{"start"}, services...)...)
}

func (d dockerCompose) PsFormat(format string, services []string) (string, error) {
	full := append(append([]string{}, d.cmd...), "ps", "--format", format)
	full = append(full, services...)
	c := exec.Command(full[0], full[1:]...)
	out, err := c.CombinedOutput()
	return string(out), err
}

// binMigrator runs the asserted pre-boot migration step via
// `bin/apply-migrations`, resolved against the repo root so it works from
// any CWD. Refusing to boot on a stale schema is enforced by Run, not
// here.
type binMigrator struct {
	root string
}

func (m binMigrator) Apply() error {
	root := m.root
	if root == "" {
		root = resolveRepoRoot()
	}
	cmd := exec.Command("bin/apply-migrations")
	if root != "" {
		cmd.Dir = root
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// httpGetter implements HTTPGetter via net/http with an 8s timeout
// (matching the shell's --max-time 8).
type httpGetter struct{ client *http.Client }

func (h httpGetter) Get(url string) (int, string, error) {
	resp, err := h.client.Get(url)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, "", err
	}
	return resp.StatusCode, string(body), nil
}

// herdKickFleet re-engages the standing fleet via the Go kick package
// after a healthy activation. Mirrors `bin/herd-kick` invocation, run
// against the resolved repo root so `herd kick` resolves the fleet
// config from any CWD.
type herdKickFleet struct {
	root string
}

func (f herdKickFleet) Kick(reason string) error {
	root := f.root
	if root == "" {
		root = resolveRepoRoot()
	}
	cmd := exec.Command("herd", "kick", "--reason", reason)
	if root != "" {
		cmd.Dir = root
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
