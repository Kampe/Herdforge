// Package feedback ports bin/herd-feedback: a periodic fleet-wide
// control-plane feedback census.
//
// The coordinator only ever sees what it thought to ask about. This asks every
// live lane the questions the coordinator cannot ask itself — what is blocked,
// what capacity is idle, which prompt never landed, where quota disagrees with
// pane status, and what the coordinator missed entirely.
//
// Durable inbox delivery is authoritative; a settled agent additionally gets a
// wake nudge, but a failed nudge is a warning, never a lost request. Replies
// are read from the coordinator's real bin/herd-mail-shaped inbox (see
// reply.go) — not pkg/mail's Go-native envelope shape, whose "sender"/
// "subject" JSON keys would silently miss every reply a lane actually writes.
package feedback

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Kampe/Herdforge/pkg/herdr"
	"github.com/Kampe/Herdforge/pkg/posture"
	"github.com/Kampe/Herdforge/pkg/winddown"
)

const (
	// DefaultInterval is how often a new census opens.
	DefaultInterval = 30 * time.Minute
	// DefaultGrace is how long a lane has to reply before it is reported missing.
	DefaultGrace = 10 * time.Minute
	// SubjectPrefix marks a census request and its replies.
	SubjectPrefix = "FLEET_FEEDBACK"
	// DefaultCoordinator is used when no live agent name matches
	// coordinator|orchestrator — the literal chainseer source default,
	// preserved for byte-compatible behavior.
	DefaultCoordinator = "chainseer-orchestrator"

	// Env var names honored verbatim from bin/herd-feedback.
	EnvInterval       = "HERD_FEEDBACK_INTERVAL"
	EnvGrace          = "HERD_FEEDBACK_GRACE"
	EnvFeedbackDir    = "HERD_FEEDBACK_DIR"
	EnvMailDir        = "HERD_MAIL_DIR"
	EnvWindDown       = "HERD_WIND_DOWN_SENTINEL"
	EnvSendBin        = "HERD_SEND_BIN"
	EnvWorkspace      = "HERD_WORKSPACE"
	EnvWorkspaceLabel = "HERD_WORKSPACE_LABEL"
)

// CensusState is the durable census record.
type CensusState struct {
	Epoch            string   `json:"epoch"`
	RequestedAtEpoch int64    `json:"requested_at_epoch"`
	Lanes            []string `json:"lanes"`
}

// StatePath is the durable census file.
func StatePath(stateDir string) string {
	if strings.TrimSpace(stateDir) == "" {
		stateDir = filepath.Join(".herd", "feedback")
	}
	return filepath.Join(stateDir, "current.json")
}

// Load reads the durable census, treating absence as "no census yet". A
// corrupt file is an error, not an implicit fresh start: silently discarding
// it would drop the outstanding request set and report a false all-clear.
func Load(stateDir string) (*CensusState, error) {
	path := StatePath(stateDir)
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &CensusState{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("feedback: read %s: %w", path, err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return &CensusState{}, nil
	}
	var s CensusState
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("feedback: corrupt census %s: %w", path, err)
	}
	return &s, nil
}

// Save writes the census atomically so a crash mid-write cannot leave a
// half-parsed outstanding set behind.
func Save(stateDir string, s *CensusState) error {
	path := StatePath(stateDir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("feedback: create state dir: %w", err)
	}
	body, err := json.Marshal(s)
	if err != nil {
		return fmt.Errorf("feedback: marshal census: %w", err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o600); err != nil {
		return fmt.Errorf("feedback: write census: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("feedback: commit census: %w", err)
	}
	return nil
}

// Epoch stamps a census. Callers pass the clock so this stays testable.
func Epoch(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

// Missing returns requested lanes that have not replied, deterministically
// sorted so two runs of the same state produce identical output.
func Missing(requested, replied []string) []string {
	got := make(map[string]bool, len(replied))
	for _, r := range replied {
		got[r] = true
	}
	var missing []string
	for _, want := range requested {
		if !got[want] {
			missing = append(missing, want)
		}
	}
	sort.Strings(missing)
	return missing
}

// Due reports whether a new census should open.
func Due(last time.Time, now time.Time, interval time.Duration) bool {
	if last.IsZero() {
		return true
	}
	return now.Sub(last) >= interval
}

// Overdue reports whether outstanding replies have blown the grace window.
func Overdue(requestedAt time.Time, now time.Time, grace time.Duration, missing int) bool {
	if missing == 0 || requestedAt.IsZero() {
		return false
	}
	return now.Sub(requestedAt) >= grace
}

// RequestBody is the exact census prompt. It names the reply command so a lane
// cannot answer in a shape the census is unable to count, and it demands an
// explicit NONE per field so silence is never mistaken for "nothing to report".
func RequestBody(epoch, coordinator string) string {
	return fmt.Sprintf(
		"FLEET FEEDBACK REQUEST %s. Before your next handoff, inspect beyond your assigned task and report: "+
			"blocker or underutilized capacity; any prompt that was not consumed; quota/provider state that "+
			"disagrees with pane status; and anything the coordinator or herd tooling missed. "+
			"Reply exactly with: HERD_LANE=<your-lane> bin/herd-mail send %s --summary 'FLEET_FEEDBACK %s <your-lane>' "+
			"'blocker=<...>; delivery=<...>; quota=<...>; coordinator_blind_spot=<...>' "+
			"Use NONE explicitly for each empty field. Do not mutate outside your assigned worktree.",
		epoch, coordinator, epoch)
}

// Subject is the census subject for one epoch.
func Subject(epoch string) string { return SubjectPrefix + " " + epoch }

// NeedsWake reports whether a settled lane should also get a nudge. A working
// or starting agent is left alone: the durable inbox copy already exists and
// interrupting active work to deliver a census is worse than waiting.
func NeedsWake(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "idle", "done", "blocked", "unknown", "":
		return true
	}
	return false
}

// Options configures one Run. Zero-value fields resolve from the same
// environment variables and repo-relative defaults bin/herd-feedback used.
type Options struct {
	Interval int // seconds; <=0 resolves from HERD_FEEDBACK_INTERVAL, default 1800
	Grace    int // seconds; <=0 resolves from HERD_FEEDBACK_GRACE, default 600

	StateDir string // <=empty resolves from HERD_FEEDBACK_DIR, default <StateDir>/feedback
	MailDir  string // empty resolves from HERD_MAIL_DIR, default <StateDir>/mail
	WindDown string // sentinel file path; empty resolves from HERD_WIND_DOWN_SENTINEL
	SendBin  string // empty resolves from HERD_SEND_BIN; still empty uses herdr's native wake

	RepoRoot       string // repo root for workspace label resolution; default "."
	Workspace      string // pre-resolved workspace id; empty resolves via ResolveWorkspace
	WorkspaceLabel string // empty resolves from HERD_WORKSPACE_LABEL
	Coordinator    string // pre-resolved coordinator; empty resolves via CoordinatorTarget

	Now            func() time.Time
	ListWorkspaces func() ([]herdr.WorkspaceEntry, error)
	ListAgents     func() ([]herdr.AgentEntry, error)
	DurableMail    func(ctx context.Context, to, summary, body string) error
	Wake           func(ctx context.Context, lane, nudge string) error

	// AdmissionGate governs whether a NEW census may open (the report of an
	// outstanding census is never gated — only sending fresh requests is).
	// Defaults to the fleet's one production posture gate (pkg/winddown,
	// ".herd/winddown.json" or HERD_WINDDOWN_STATE) so `herd winddown on`
	// also stops this census from waking idle lanes.
	AdmissionGate func(ctx context.Context) error

	Stdout, Stderr io.Writer
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func (o *Options) setDefaults() {
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Interval <= 0 {
		o.Interval = envInt(EnvInterval, int(DefaultInterval.Seconds()))
	}
	if o.Grace <= 0 {
		o.Grace = envInt(EnvGrace, int(DefaultGrace.Seconds()))
	}
	if strings.TrimSpace(o.RepoRoot) == "" {
		o.RepoRoot = "."
	}
	if strings.TrimSpace(o.StateDir) == "" {
		o.StateDir = strings.TrimSpace(os.Getenv(EnvFeedbackDir))
	}
	if strings.TrimSpace(o.StateDir) == "" {
		o.StateDir = filepath.Join(posture.StateDir(), "feedback")
	}
	if strings.TrimSpace(o.MailDir) == "" {
		o.MailDir = strings.TrimSpace(os.Getenv(EnvMailDir))
	}
	if strings.TrimSpace(o.MailDir) == "" {
		o.MailDir = filepath.Join(posture.StateDir(), "mail")
	}
	if strings.TrimSpace(o.WindDown) == "" {
		o.WindDown = strings.TrimSpace(os.Getenv(EnvWindDown))
	}
	if strings.TrimSpace(o.SendBin) == "" {
		o.SendBin = strings.TrimSpace(os.Getenv(EnvSendBin))
	}
	if strings.TrimSpace(o.WorkspaceLabel) == "" {
		o.WorkspaceLabel = strings.TrimSpace(os.Getenv(EnvWorkspaceLabel))
	}
	if o.ListWorkspaces == nil {
		o.ListWorkspaces = herdr.WorkspaceList
	}
	if o.ListAgents == nil {
		o.ListAgents = herdr.AgentList
	}
	if o.AdmissionGate == nil {
		o.AdmissionGate = func(ctx context.Context) error { return winddown.RequireAdmission(ctx, "") }
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
}

// Selftest verifies the load-bearing commitments this port must preserve:
// the outbound request still carries the FLEET_FEEDBACK subject and names
// herd-mail as the durable reply channel, and HERD_FEEDBACK_INTERVAL is
// actually read by the interval resolver — a behavioral check, not a
// literal-vs-literal comparison that can only fail by editing both sides.
// It is the in-process equivalent of bin/herd-feedback's own `grep` selftest.
func Selftest() error {
	body := RequestBody("E1", "coordinator")
	if !strings.Contains(body, SubjectPrefix) {
		return fmt.Errorf("request body lost the %s subject", SubjectPrefix)
	}
	if !strings.Contains(body, "herd-mail") {
		return fmt.Errorf("request body lost the herd-mail reply command")
	}
	old, had := os.LookupEnv(EnvInterval)
	os.Setenv(EnvInterval, "4242")
	got := envInt(EnvInterval, 1800)
	if had {
		os.Setenv(EnvInterval, old)
	} else {
		os.Unsetenv(EnvInterval)
	}
	if got != 4242 {
		return fmt.Errorf("%s is not honored by the interval resolver", EnvInterval)
	}
	return nil
}

// ErrWorkspaceUnresolved is returned when the herdr workspace cannot be
// identified. A census over an unresolved workspace would report a false
// empty fleet, so Run refuses rather than proceeding with zero lanes.
var ErrWorkspaceUnresolved = fmt.Errorf("workspace unresolved; refusing a false empty census")

// Run evaluates and, if due, opens one census: report the outstanding
// census (if any), then — when the interval has elapsed and no wind-down
// sentinel is present — request fresh feedback from every non-coordinator
// lane in the workspace.
func Run(ctx context.Context, opts Options) error {
	opts.setDefaults()

	workspace := strings.TrimSpace(opts.Workspace)
	if workspace == "" {
		var err error
		workspace, err = ResolveWorkspace(opts.RepoRoot, opts.WorkspaceLabel, os.Getenv(EnvWorkspace), opts.ListWorkspaces)
		if err != nil {
			fmt.Fprintln(opts.Stderr, "herd-feedback: workspace unresolved; refusing a false empty census")
			return ErrWorkspaceUnresolved
		}
	}

	coordinator := strings.TrimSpace(opts.Coordinator)
	if coordinator == "" {
		if agents, err := opts.ListAgents(); err == nil {
			coordinator = CoordinatorTarget(agents, workspace)
		}
		if coordinator == "" {
			coordinator = DefaultCoordinator
		}
	}

	if err := os.MkdirAll(opts.StateDir, 0o700); err != nil {
		return fmt.Errorf("herd-feedback: create state dir: %w", err)
	}
	state, err := Load(opts.StateDir)
	if err != nil {
		return err
	}

	now := opts.Now()
	if state.Epoch != "" {
		mailFile := filepath.Join(opts.MailDir, coordinator+".jsonl")
		replied, missing, rerr := ReplyFromLanes(mailFile, state.Epoch, state.Lanes)
		if rerr != nil {
			return fmt.Errorf("herd-feedback: reply census: %w", rerr)
		}
		fmt.Fprintf(opts.Stdout, "herd-feedback: epoch=%s replies=%d/%d missing=%s\n",
			state.Epoch, len(replied), len(state.Lanes), strings.Join(missing, ","))
		requestedAt := time.Unix(state.RequestedAtEpoch, 0).UTC()
		if Overdue(requestedAt, now, time.Duration(opts.Grace)*time.Second, len(missing)) {
			fmt.Fprintf(opts.Stderr, "herd-feedback: FEEDBACK_MISSING after %ds: %s\n", opts.Grace, strings.Join(missing, ","))
		}
	}

	last := time.Time{}
	if state.RequestedAtEpoch > 0 {
		last = time.Unix(state.RequestedAtEpoch, 0).UTC()
	}
	if !Due(last, now, time.Duration(opts.Interval)*time.Second) {
		return nil
	}

	if opts.WindDown != "" {
		if _, serr := os.Stat(opts.WindDown); serr == nil {
			fmt.Fprintln(opts.Stdout, "herd-feedback: wind-down active, not starting a new census")
			return nil
		}
	}

	if opts.AdmissionGate != nil {
		if aerr := opts.AdmissionGate(ctx); aerr != nil {
			fmt.Fprintf(opts.Stdout, "herd-feedback: fleet admission rejected, not starting a new census: %v\n", aerr)
			return nil
		}
	}

	// A failed enumeration is NOT the same as a genuinely empty fleet: opening
	// and persisting a zero-lane census here would report 0/0 "fully replied"
	// forever afterward, silently masking the outage instead of surfacing it.
	// Skip this cycle (Run still returns nil — a missing/failing herdr must
	// never fail the whole command) and retry the request on the next tick.
	agents, err := opts.ListAgents()
	if err != nil {
		fmt.Fprintf(opts.Stderr, "herd-feedback: WARN agent list unavailable, not opening a new census this cycle: %v\n", err)
		return nil
	}

	durableMail := opts.DurableMail
	if durableMail == nil {
		durableMail = DefaultDurableMail(opts.MailDir, coordinator)
	}
	wake := opts.Wake
	if wake == nil {
		wake = DefaultWake(opts.SendBin)
	}

	epoch := Epoch(now)
	body := RequestBody(epoch, coordinator)
	subject := Subject(epoch)

	var lanes []string
	for _, a := range agents {
		if a.Workspace != workspace || strings.TrimSpace(a.Name) == "" || a.Name == coordinator {
			continue
		}
		// Durable delivery is the authoritative half of the census: a
		// census that cannot fuse a lane's inbox is a false census, so a
		// failed durable send is fatal rather than silently dropping the
		// lane from tracking.
		if err := durableMail(ctx, a.Name, subject, body); err != nil {
			fmt.Fprintf(opts.Stderr, "herd-feedback: durable send to %s failed: %v\n", a.Name, err)
			return fmt.Errorf("herd-feedback: durable send to %s: %w", a.Name, err)
		}
		lanes = append(lanes, a.Name)
		if NeedsWake(a.Status) {
			nudge := fmt.Sprintf("Read and answer the %s request in your durable inbox before taking more work.", subject)
			if err := wake(ctx, a.Name, nudge); err != nil {
				fmt.Fprintf(opts.Stderr, "herd-feedback: WARN could not prove wake delivery to %s (durable inbox copy exists): %v\n", a.Name, err)
			}
		}
	}

	if err := Save(opts.StateDir, &CensusState{Epoch: epoch, RequestedAtEpoch: now.Unix(), Lanes: lanes}); err != nil {
		return err
	}
	fmt.Fprintf(opts.Stdout, "herd-feedback: requested epoch=%s lanes=%d durable=yes\n", epoch, len(lanes))
	return nil
}
