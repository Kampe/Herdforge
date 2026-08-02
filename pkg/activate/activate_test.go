package activate

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// --- Pure predicate fixtures (mirror bin/herd-activate --selftest) ---

const okStatusJSON = `{"data":{"status":"ok","deployables":[{"name":"api","status":"ok"},{"name":"indexer-evm","status":"ok"}]}}`
const degradedStatusJSON = `{"data":{"status":"degraded","deployables":[{"name":"api","status":"ok"},{"name":"indexer-evm","status":"down"}]}}`

var defaultDeployables = []string{"api", "core", "indexer-evm", "indexer-other", "worker"}

func TestStatusUnhealthy_AllOK(t *testing.T) {
	got := StatusUnhealthy(okStatusJSON)
	if got != "" {
		t.Fatalf("expected empty for all-ok, got %q", got)
	}
}

func TestStatusUnhealthy_Degraded(t *testing.T) {
	got := StatusUnhealthy(degradedStatusJSON)
	if got != "indexer-evm" {
		t.Fatalf("expected indexer-evm, got %q", got)
	}
}

func TestStatusUnhealthy_EmptyOrUnparseable(t *testing.T) {
	if got := StatusUnhealthy(""); got != "" {
		t.Fatalf("expected empty for blank input, got %q", got)
	}
	if got := StatusUnhealthy("not-json"); got != "" {
		t.Fatalf("expected empty for unparseable input, got %q", got)
	}
}

func TestStatusUnhealthy_NoDataField(t *testing.T) {
	// Defensive: missing data.deployables must not panic, must return empty.
	if got := StatusUnhealthy(`{"data":{}}`); got != "" {
		t.Fatalf("expected empty for missing deployables, got %q", got)
	}
}

func TestComposeNotRunning_CreatedState(t *testing.T) {
	// Live incident shape: core/worker Created, others running. Must NOT pass.
	sample := "api running\ncore created\nindexer-evm running\nindexer-other running\nworker created\n"
	got := ComposeNotRunning(sample, defaultDeployables)
	if got != "core:created,worker:created" {
		t.Fatalf("expected core:created,worker:created, got %q", got)
	}
}

func TestComposeNotRunning_AllRunning(t *testing.T) {
	sample := "api running\ncore running\nindexer-evm running\nindexer-other running\nworker running\n"
	got := ComposeNotRunning(sample, defaultDeployables)
	if got != "" {
		t.Fatalf("expected empty when all running, got %q", got)
	}
}

func TestComposeNotRunning_Missing(t *testing.T) {
	got := ComposeNotRunning("api running\n", []string{"api", "core"})
	if got != "core:missing" {
		t.Fatalf("expected core:missing, got %q", got)
	}
}

func TestComposeNotRunning_BlankLinesIgnored(t *testing.T) {
	sample := "api running\n\ncore running\n\n"
	got := ComposeNotRunning(sample, []string{"api", "core"})
	if got != "" {
		t.Fatalf("expected empty, blank lines must be ignored, got %q", got)
	}
}

func TestComposeNotRunning_EmptyInput(t *testing.T) {
	got := ComposeNotRunning("", []string{"api", "core"})
	if got != "api:missing,core:missing" {
		t.Fatalf("expected both missing, got %q", got)
	}
}

// --- Selftest ---

func TestSelftest_Pass(t *testing.T) {
	if err := Selftest(); err != nil {
		t.Fatalf("selftest should pass: %v", err)
	}
}

// --- Orchestrator with fake clients ---

type fakeCompose struct {
	buildErr    error
	upErr       error
	startErr    error
	psText      string
	psErr       error
	buildCalled []string
	upCalled    []string
	startCalled bool
	upDetached  bool
}

func (f *fakeCompose) Build(services []string) error {
	f.buildCalled = append(f.buildCalled, services...)
	return f.buildErr
}

func (f *fakeCompose) Up(detached bool, services []string) error {
	f.upDetached = detached
	f.upCalled = services
	return f.upErr
}

func (f *fakeCompose) Start(services []string) error {
	f.startCalled = true
	return f.startErr
}

func (f *fakeCompose) PsFormat(format string, services []string) (string, error) {
	return f.psText, f.psErr
}

type fakeMigrator struct {
	err    error
	called bool
}

func (f *fakeMigrator) Apply() error {
	f.called = true
	return f.err
}

type fakeHTTP struct {
	code  int
	body  string
	err   error
	calls int
}

func (f *fakeHTTP) Get(url string) (int, string, error) {
	f.calls++
	return f.code, f.body, f.err
}

type fakeFleet struct {
	kicked bool
	reason string
	err    error
}

func (f *fakeFleet) Kick(reason string) error {
	f.kicked = true
	f.reason = reason
	return f.err
}

func baseOpts() Options {
	return Options{
		Deployables:  defaultDeployables,
		APIURL:       "http://localhost:13100",
		WebURL:       "http://localhost:4174",
		Timeout:      200 * time.Millisecond,
		PollInterval: 50 * time.Millisecond,
	}
}

func TestRun_MigrationFailure_RefusesToBoot(t *testing.T) {
	compose := &fakeCompose{psText: allRunningPS()}
	mig := &fakeMigrator{err: errors.New("schema mismatch")}
	http := &fakeHTTP{code: 200, body: okStatusJSON}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err == nil {
		t.Fatal("expected error when migrations fail")
	}
	if res != nil {
		t.Fatalf("expected nil result on migration failure, got %+v", res)
	}
	if !mig.called {
		t.Fatal("migrator must be called before boot")
	}
	if len(compose.upCalled) != 0 {
		t.Fatal("must NOT compose up when migrations fail — refusing to boot on stale schema")
	}
	if fleet.kicked {
		t.Fatal("fleet must not be kicked on migration failure")
	}
}

func TestRun_AllHealthy_KicksFleet(t *testing.T) {
	compose := &fakeCompose{psText: allRunningPS()}
	mig := &fakeMigrator{}
	http := &fakeHTTP{code: 200, body: okStatusJSON}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy, got %+v", res)
	}
	if res.Overall != "ok" {
		t.Fatalf("expected overall=ok, got %q", res.Overall)
	}
	if res.Unhealthy != "" {
		t.Fatalf("expected no unhealthy, got %q", res.Unhealthy)
	}
	if res.NotRunning != "" {
		t.Fatalf("expected none not-running, got %q", res.NotRunning)
	}
	if !compose.upDetached {
		t.Fatal("must compose up -d (detached)")
	}
	if !fleet.kicked {
		t.Fatal("fleet must be kicked on healthy activation")
	}
	if !strings.Contains(fleet.reason, "post-activation") {
		t.Fatalf("kick reason should mention post-activation, got %q", fleet.reason)
	}
}

func TestRun_AllHealthy_NoFleetSuppressed(t *testing.T) {
	compose := &fakeCompose{psText: allRunningPS()}
	mig := &fakeMigrator{}
	http := &fakeHTTP{code: 200, body: okStatusJSON}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.NoFleet = true
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Healthy {
		t.Fatalf("expected healthy, got %+v", res)
	}
	if fleet.kicked {
		t.Fatal("NoFleet must suppress the standing-fleet kick (wind-down resurrection guard)")
	}
}

func TestRun_ComposeNotRunning_TriesStartThenUnhealthy(t *testing.T) {
	// First poll: worker created. Start attempted. Still never healthy within timeout.
	compose := &fakeCompose{psText: "api running\ncore running\nindexer-evm running\nindexer-other running\nworker created\n"}
	mig := &fakeMigrator{}
	http := &fakeHTTP{code: 200, body: okStatusJSON}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err == nil {
		t.Fatal("expected error when unhealthy after timeout")
	}
	if res == nil {
		t.Fatal("expected non-nil result reflecting unhealthy state")
	}
	if res.Healthy {
		t.Fatal("must not be healthy when compose not all running")
	}
	if !compose.startCalled {
		t.Fatal("must attempt compose start (not recreate) when containers in Created state")
	}
	if fleet.kicked {
		t.Fatal("must not kick fleet when unhealthy")
	}
	if !strings.Contains(res.NotRunning, "worker:created") {
		t.Fatalf("expected NotRunning to contain worker:created, got %q", res.NotRunning)
	}
}

func TestRun_APIUnreachable_Unhealthy(t *testing.T) {
	compose := &fakeCompose{psText: allRunningPS()}
	mig := &fakeMigrator{}
	http := &fakeHTTP{err: errors.New("connection refused")}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err == nil {
		t.Fatal("expected error when API unreachable after timeout")
	}
	if res == nil || res.Healthy {
		t.Fatal("must be unhealthy when API unreachable")
	}
	if res.Overall != "unreachable" {
		t.Fatalf("expected overall=unreachable, got %q", res.Overall)
	}
	if res.Unhealthy != "api-unreachable" {
		t.Fatalf("expected unhealthy=api-unreachable, got %q", res.Unhealthy)
	}
	if fleet.kicked {
		t.Fatal("must not kick fleet when API unreachable")
	}
}

func TestRun_StatusDegraded_Unhealthy(t *testing.T) {
	compose := &fakeCompose{psText: allRunningPS()}
	mig := &fakeMigrator{}
	http := &fakeHTTP{code: 200, body: degradedStatusJSON}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err == nil {
		t.Fatal("expected error when status degraded after timeout")
	}
	if res == nil || res.Healthy {
		t.Fatal("must be unhealthy when /v1/status degraded")
	}
	if res.Overall != "degraded" {
		t.Fatalf("expected overall=degraded, got %q", res.Overall)
	}
	if res.Unhealthy != "indexer-evm" {
		t.Fatalf("expected unhealthy=indexer-evm, got %q", res.Unhealthy)
	}
	if fleet.kicked {
		t.Fatal("must not kick fleet when degraded")
	}
}

func TestRun_BuildServicesFirst(t *testing.T) {
	compose := &fakeCompose{psText: allRunningPS()}
	mig := &fakeMigrator{}
	http := &fakeHTTP{code: 200, body: okStatusJSON}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.BuildServices = []string{"api", "worker"}
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	res, err := Run(opts)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Healthy {
		t.Fatal("expected healthy")
	}
	if len(compose.buildCalled) != 2 || compose.buildCalled[0] != "api" || compose.buildCalled[1] != "worker" {
		t.Fatalf("expected build of [api worker], got %v", compose.buildCalled)
	}
}

func TestRun_BuildFailure_AbortsBeforeBoot(t *testing.T) {
	compose := &fakeCompose{buildErr: errors.New("docker build failed")}
	mig := &fakeMigrator{}
	http := &fakeHTTP{}
	fleet := &fakeFleet{}

	opts := baseOpts()
	opts.BuildServices = []string{"api"}
	opts.Compose = compose
	opts.Migrator = mig
	opts.HTTP = http
	opts.Fleet = fleet

	_, err := Run(opts)
	if err == nil {
		t.Fatal("expected build failure to abort")
	}
	if !strings.Contains(err.Error(), "build") {
		t.Fatalf("error should mention build, got %v", err)
	}
	if len(compose.upCalled) != 0 {
		t.Fatal("must not compose up after build failure")
	}
	if mig.called {
		t.Fatal("build fails first; migrator must NOT run after a build failure (build -> migrations -> up order)")
	}
}

// allRunningPS returns compose ps text with every default deployable running.
func allRunningPS() string {
	var b strings.Builder
	for _, d := range defaultDeployables {
		b.WriteString(d)
		b.WriteString(" running\n")
	}
	return b.String()
}
