package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kampe/Herdforge/pkg/router"
	"github.com/Kampe/Herdforge/pkg/usage"
)

// TestPoolReviewerIsNotHardwiredToOneHarness is the FAC-574 gate.
//
// The pool launched OpenCode with an Ollama proxy model unconditionally, so no
// native-Claude route existed through the exact-SHA pool lifecycle and a rate
// limited proxy killed exact review entirely. A reviewer harness must come from
// the router, never from a literal in this file.
func TestPoolReviewerIsNotHardwiredToOneHarness(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	// Comments legitimately name the old default when explaining the fix, so
	// only inspect code lines.
	for i, line := range strings.Split(body, "\n") {
		code := strings.TrimSpace(line)
		if code == "" || strings.HasPrefix(code, "//") {
			continue
		}
		for _, banned := range []string{"litellm/", "ollama", "\"opencode\""} {
			if strings.Contains(strings.ToLower(code), banned) {
				t.Errorf("review_pool.go:%d hardwires a reviewer surface (%q): %s", i+1, banned, code)
			}
		}
	}
	if !strings.Contains(body, "router.NewRouter") {
		t.Error("pool reviewer must resolve its harness through the router")
	}
}

// A model without its surface is not a route; accepting it would silently pair
// an operator's model with whatever provider the router happened to pick.
func TestModelWithoutProviderIsRejected(t *testing.T) {
	if _, err := resolvePoolReviewer("", "claude-sonnet-5", ""); err == nil {
		t.Fatal("expected --model without --provider to be refused")
	}
}

// TestRouteAndPoolReviewShareAuthoritativeHealthyDecision reproduces the
// 2026-08-30 W4 split on the shipped paths. The route command observes a
// healthy Claude/Sonnet tuple, then the live source changes to UNKNOWN before
// review resolves the exact tuple in a separate process. Both commands must
// consume the same fresh persisted authority reading, so review cannot reject
// what route just advertised. Before FAC-627, route fetched live without
// publishing the reading while review fetched independently through the cache;
// this test then failed with UNKNOWN no-quota-data.
func TestRouteAndPoolReviewShareAuthoritativeHealthyDecision(t *testing.T) {
	healthy := `{"generatedAt":"2026-08-30T08:00:00Z","providers":{"claude":{"displayName":"claude","stale":false,"resources":{"weekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	unknown := `{"generatedAt":"2026-08-30T08:00:05Z","providers":{"claude":{"displayName":"claude","stale":false,"resources":{}}}}`
	fixture := installExactReviewRouteFixture(t, healthy, "")
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	t.Setenv("HERD_WORKSPACE", "w4")
	t.Setenv("HERD_ROOT", fixture.dir)

	var stdout, stderr bytes.Buffer
	if err := runRouteCommand([]string{"qa", "--provider", "claude", "--exclude-family", "openai", "--json"}, &stdout, &stderr); err != nil {
		t.Fatalf("healthy shipped route command refused Claude/Sonnet: %v\nstderr=%s", err, stderr.String())
	}
	var advertised router.Route
	if err := json.Unmarshal(stdout.Bytes(), &advertised); err != nil {
		t.Fatalf("decode shipped route JSON: %v\n%s", err, stdout.String())
	}
	if advertised.Provider != "claude" || advertised.Model != "claude-sonnet-5" || advertised.Task != reviewerRouteTaskShape {
		t.Fatalf("advertised route = %+v, want qa claude/claude-sonnet-5", advertised)
	}
	if advertised.DecisionAuthority != reviewerRouteDecisionAuthority || advertised.Workspace != "w4" || advertised.CacheAge != "0s" {
		t.Fatalf("route authority metadata = %+v, want shared authority, workspace w4, fresh cache", advertised)
	}
	var exactStdout bytes.Buffer
	if err := runRouteCommand([]string{
		"qa", "--provider", "claude", "--model", "claude-sonnet-5",
		"--exclude-family", "openai", "--json",
	}, &exactStdout, &stderr); err != nil {
		t.Fatalf("exact tuple advertised healthy but explicit route pin was refused: %v", err)
	}
	var exact router.Route
	if err := json.Unmarshal(exactStdout.Bytes(), &exact); err != nil {
		t.Fatalf("decode exact pinned route JSON: %v", err)
	}
	if exact.Provider != advertised.Provider || exact.Model != advertised.Model || exact.QuotaPool != advertised.QuotaPool || exact.DecisionAuthority != advertised.DecisionAuthority {
		t.Fatalf("explicit route pin used a different authority: advertised=%+v exact=%+v", advertised, exact)
	}

	// If review consults a second authority, it sees UNKNOWN and reproduces the
	// live refusal. A correct implementation reads the fresh decision snapshot
	// persisted by the route command, even across this process boundary.
	writeQuotaStub(t, fixture.openusage, unknown)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFAC627ReviewRouteSubprocess$")
	cmd.Env = append(os.Environ(),
		"HERD_FAC627_HELPER=1",
		"HERD_FAC627_WORKSPACE=w4",
		"HERD_FAC627_ROOT="+fixture.dir,
		"HERD_FAC627_HERDR_BIN="+fixture.herdr,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("review path refused the tuple advertised seconds earlier: %v\n%s", err, out)
	}
	for _, want := range []string{
		"FAC627_REVIEW_RESULT", "provider=claude", "model=claude-sonnet-5",
		"pool=default", "family=anthropic",
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("review subprocess missing %q:\n%s", want, out)
		}
	}
	fetches, err := os.ReadFile(fixture.quotaFetchLog)
	if err != nil {
		t.Fatalf("read quota fetch log: %v", err)
	}
	if got := len(strings.Fields(string(fetches))); got != 1 {
		t.Fatalf("route and review consulted different quota authorities: fetches=%d log=%q", got, fetches)
	}
}

func TestLiveRouteCountScopesExplicitWorkspace(t *testing.T) {
	dir := t.TempDir()
	unexpected := filepath.Join(dir, "unexpected.log")
	bin := filepath.Join(dir, "herdr")
	script := `#!/bin/sh
if [ "$1 $2" = "agent list" ]; then
  printf '%s\n' '{"result":{"agents":[{"name":"other-workspace","agent":"claude","agent_status":"working","pane_id":"w5:p1","workspace_id":"w5"}]}}'
  exit 0
fi
printf '%s\n' "$*" >> "$HERD_TEST_UNEXPECTED_HERDR"
exit 1
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HERDR_BIN_PATH", bin)
	t.Setenv("HERD_WORKSPACE", "w4")
	t.Setenv("HERD_TEST_UNEXPECTED_HERDR", unexpected)
	got, err := liveRouteCount("claude", "claude-sonnet-5", "default")
	if err != nil {
		t.Fatalf("workspace-scoped live count: %v", err)
	}
	if got != 0 {
		t.Fatalf("W4 route counted %d matching agents from W5", got)
	}
	if _, err := os.Stat(unexpected); !os.IsNotExist(err) {
		t.Fatalf("workspace filter inspected a foreign pane: %v", err)
	}
}

// Helper subprocess for the cross-process production-path regression above.
// TestMain deliberately strips inherited lane identity, so restore only the
// exact fixture authority fields under explicit test-only names.
func TestFAC627ReviewRouteSubprocess(t *testing.T) {
	if os.Getenv("HERD_FAC627_HELPER") != "1" {
		return
	}
	t.Setenv("HERD_WORKSPACE", os.Getenv("HERD_FAC627_WORKSPACE"))
	t.Setenv("HERD_ROOT", os.Getenv("HERD_FAC627_ROOT"))
	t.Setenv("HERDR_BIN_PATH", os.Getenv("HERD_FAC627_HERDR_BIN"))
	t.Setenv("HERD_USE_PI", "0")
	got, err := resolvePoolReviewer("claude", "claude-sonnet-5", "openai")
	if err != nil {
		t.Fatalf("FAC627_REVIEW_ERROR=%v", err)
	}
	if got.QuotaAge <= 0 || got.QuotaAge >= 45*time.Second {
		t.Fatalf("FAC627_REVIEW_ERROR=cache age %s is not the route command's fresh persisted reading", got.QuotaAge)
	}
	fmt.Printf("FAC627_REVIEW_RESULT provider=%s model=%s pool=%s family=%s cache_age=%s\n",
		got.Provider, got.Model, got.Pool, got.Family, got.QuotaAge.Round(time.Second))
}

// An explicit model is the route, not a string replacement applied after the
// provider's default model has already passed routing. AGY's non-Gemini and
// Gemini pools are independently metered: an exhausted non-Gemini default for
// QA must not reject a healthy exact Gemini request, and the live probe must
// target the exact Gemini model rather than a healthy sibling.
func TestExactReviewModelOverrideUsesItsOwnHealthyPool(t *testing.T) {
	usage.InvalidateSnapshotCache()
	t.Cleanup(usage.InvalidateSnapshotCache)

	dir := t.TempDir()
	probeLog := filepath.Join(dir, "provider-probes.log")
	agy := filepath.Join(dir, "agy")
	if err := os.WriteFile(agy, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HERD_TEST_PROBE_LOG\"\nprintf 'HERD_PROVIDER_PROBE_OK\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	openusage := filepath.Join(dir, "openusage")
	quota := `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"used":100,"utilization":1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	if err := os.WriteFile(openusage, []byte("#!/bin/sh\nprintf '%s\\n' '"+quota+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERD_OPENUSAGE_BIN", openusage)
	t.Setenv("HERD_QUOTA_CACHE_PATH", filepath.Join(dir, "quota-cache.json"))
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HERD_TEST_PROBE_LOG", probeLog)
	t.Setenv("HERDR_ROUTE_STATE_DIR", filepath.Join(dir, "routing-state"))
	t.Setenv("HERD_AVAILABLE_PROVIDERS", "")
	t.Setenv("HERD_UNAVAILABLE_PROVIDERS", "")
	t.Setenv("HERD_FAMILY_POSTURE", "")

	got, err := resolvePoolReviewer("agy", "gemini-3.7-flash", "")
	if err != nil {
		t.Fatalf("healthy exact override was rejected by the exhausted default pool: %v", err)
	}
	if got.Provider != "agy" || got.Model != "gemini-3.7-flash" || got.Family != "google" {
		t.Fatalf("resolved reviewer = %+v, want exact AGY Gemini/Google identity", got)
	}
	if pool := router.QuotaPoolFor(got.Provider, got.Model); pool != "gemini" {
		t.Fatalf("exact override bills pool %q, want gemini", pool)
	}
	probes, err := os.ReadFile(probeLog)
	if err != nil {
		t.Fatalf("exact live probe was not run: %v", err)
	}
	if !strings.Contains(string(probes), "--model gemini-3.7-flash") {
		t.Fatalf("live probe did not evaluate the exact override: %s", probes)
	}
	if strings.Contains(string(probes), "gemini-3.1-pro-high") || strings.Contains(string(probes), "claude-opus-4-6-thinking") {
		t.Fatalf("default model was probed before applying the exact override: %s", probes)
	}
	if _, err := os.Stat(filepath.Join(dir, "pool")); !os.IsNotExist(err) {
		t.Fatalf("route evaluation created a pool/lease side effect: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "tabs.log")); !os.IsNotExist(err) {
		t.Fatalf("route evaluation created a tab side effect: %v", err)
	}
}

func TestExactReviewModelOverrideRefusalsAreFailClosed(t *testing.T) {
	healthy := `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	cases := []struct {
		name          string
		quota         string
		providerProbe string
		excludeFamily string
		wantReason    string
	}{
		{name: "unknown quota", quota: `{"generatedAt":"2026-08-29T20:00:00Z","providers":{}}`, wantReason: "UNKNOWN quota"},
		{name: "no quota data", quota: `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{}}}}`, wantReason: "UNKNOWN quota"},
		{name: "unavailable exact model", quota: healthy, providerProbe: "case \"$*\" in *gemini-3.7-flash*) printf 'no configured model\\n' ;; *) printf 'HERD_PROVIDER_PROBE_OK\\n' ;; esac", wantReason: "no configured model"},
		{name: "excluded exact family", quota: healthy, excludeFamily: "google", wantReason: "family google excluded"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := installExactReviewRouteFixture(t, tc.quota, tc.providerProbe)
			var routeStdout, routeStderr bytes.Buffer
			routeErr := runRouteCommand([]string{
				"qa", "--provider", "agy", "--model", "gemini-3.7-flash",
				"--exclude-family", tc.excludeFamily, "--json",
			}, &routeStdout, &routeStderr)
			if routeErr == nil {
				t.Fatalf("shipped route command advertised a tuple the launch authority must refuse: %s", routeStdout.String())
			}
			_, err := resolvePoolReviewer("agy", "gemini-3.7-flash", tc.excludeFamily)
			if err == nil {
				t.Fatal("exact route refusal unexpectedly succeeded")
			}
			for _, want := range []string{"provider=agy", "model=gemini-3.7-flash", "pool=gemini", tc.wantReason} {
				for path, got := range map[string]error{"route": routeErr, "review": err} {
					if !strings.Contains(got.Error(), want) {
						t.Errorf("%s refusal must report %q, got: %v", path, want, got)
					}
				}
			}
			if _, statErr := os.Stat(fixture.tabLog); !os.IsNotExist(statErr) {
				t.Fatalf("refused route reached a tab side effect: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.dir, "pool")); !os.IsNotExist(statErr) {
				t.Fatalf("refused route reached a pool/lease side effect: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.dir, ".herd", "pool")); !os.IsNotExist(statErr) {
				t.Fatalf("refused route reached a lease side effect: %v", statErr)
			}
			if _, statErr := os.Stat(filepath.Join(fixture.dir, ".herd", "review-packets")); !os.IsNotExist(statErr) {
				t.Fatalf("refused route reached a packet side effect: %v", statErr)
			}
		})
	}
}

func TestExactReviewModelOverrideRefreshesStaleQuotaCache(t *testing.T) {
	fresh := `{"generatedAt":"2026-08-29T20:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"used":100,"utilization":1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	stale := `{"generatedAt":"2026-08-29T19:00:00Z","providers":{"antigravity":{"displayName":"antigravity","stale":false,"resources":{"nonGeminiWeekly":{"kind":"consumption","limit":100,"remaining":90,"used":10,"utilization":0.1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800},"geminiWeekly":{"kind":"consumption","limit":100,"remaining":0,"used":100,"utilization":1,"unit":"percent","resetsAt":"2099-01-01T00:00:00Z","windowSeconds":604800}}}}}`
	fixture := installExactReviewRouteFixture(t, fresh, "")
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "45")
	cache := `{"fetched_at":"2026-08-29T18:00:00Z","snapshot":` + stale + `}`
	if err := os.WriteFile(fixture.quotaCache, []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := resolvePoolReviewer("agy", "gemini-3.7-flash", "")
	if err != nil {
		t.Fatalf("fresh live quota did not replace an aged-out refusal: %v", err)
	}
	if got.Pool != "gemini" || got.QuotaAge != 0 {
		t.Fatalf("route used pool=%q cache_age=%s, want fresh gemini evidence", got.Pool, got.QuotaAge)
	}
	if _, err := os.Stat(fixture.quotaFetchLog); err != nil {
		t.Fatalf("aged cache was used instead of a fresh quota read: %v", err)
	}
}

type exactReviewRouteFixture struct {
	dir           string
	tabLog        string
	quotaCache    string
	quotaFetchLog string
	openusage     string
	herdr         string
}

func installExactReviewRouteFixture(t *testing.T, quota, providerProbe string) exactReviewRouteFixture {
	t.Helper()
	dir := t.TempDir()
	probeLog := filepath.Join(dir, "provider-probes.log")
	if providerProbe == "" {
		providerProbe = "printf 'HERD_PROVIDER_PROBE_OK\\n'"
	}
	providerScript := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"$HERD_TEST_PROBE_LOG\"\n" + providerProbe + "\n"
	for _, provider := range []string{"agy", "claude"} {
		if err := os.WriteFile(filepath.Join(dir, provider), []byte(providerScript), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	quotaFetchLog := filepath.Join(dir, "quota-fetch.log")
	openusage := filepath.Join(dir, "openusage")
	writeQuotaStub(t, openusage, quota)
	tabLog := filepath.Join(dir, "tabs.log")
	fakeHerdr := filepath.Join(dir, "herdr")
	herdrScript := "#!/bin/sh\nif [ \"$1 $2\" = \"agent list\" ]; then\n  printf '%s\\n' '{\"result\":{\"agents\":[]}}'\n  exit 0\nfi\nprintf '%s\\n' \"$*\" >> \"$HERD_TEST_TAB_LOG\"\n"
	if err := os.WriteFile(fakeHerdr, []byte(herdrScript), 0o755); err != nil {
		t.Fatal(err)
	}
	quotaCache := filepath.Join(dir, "quota-cache.json")
	t.Setenv("PATH", dir)
	t.Setenv("HERD_USE_PI", "0")
	t.Setenv("HERD_OPENUSAGE_BIN", openusage)
	t.Setenv("HERD_QUOTA_CACHE_PATH", quotaCache)
	t.Setenv("HERD_QUOTA_CACHE_SECONDS", "0")
	t.Setenv("HERD_TEST_PROBE_LOG", probeLog)
	t.Setenv("HERD_TEST_QUOTA_LOG", quotaFetchLog)
	t.Setenv("HERD_TEST_TAB_LOG", tabLog)
	t.Setenv("HERDR_BIN_PATH", fakeHerdr)
	t.Setenv("HERDR_ROUTE_STATE_DIR", filepath.Join(dir, "routing-state"))
	t.Setenv("HERD_ROUTE_DECISION_LOG", filepath.Join(dir, "route-decisions.log"))
	t.Setenv("HERD_WORKSPACE", "w4")
	t.Setenv("HERD_ROOT", dir)
	for _, key := range []string{"HERD_AVAILABLE_PROVIDERS", "HERD_UNAVAILABLE_PROVIDERS", "HERD_FAMILY_POSTURE", "HERD_NO_GEMINI", "HERD_NO_CLAUDE", "HERD_CLAUDE_ONLY", "HERD_ERA_PROVIDERS"} {
		t.Setenv(key, "")
	}
	usage.InvalidateSnapshotCache()
	t.Cleanup(usage.InvalidateSnapshotCache)
	return exactReviewRouteFixture{
		dir: dir, tabLog: tabLog, quotaCache: quotaCache, quotaFetchLog: quotaFetchLog,
		openusage: openusage, herdr: fakeHerdr,
	}
}

func writeQuotaStub(t *testing.T, path, quota string) {
	t.Helper()
	body := "#!/bin/sh\nprintf 'fetch\\n' >> \"$HERD_TEST_QUOTA_LOG\"\nprintf '%s\\n' '" + quota + "'\n"
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// The launch must resolve before any tab or lease side effect, so an
// unroutable reviewer cannot orphan a tab or strand a warm-pool lease.
func TestHarnessResolvesBeforeSideEffects(t *testing.T) {
	src, err := os.ReadFile("review_pool.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	resolve := strings.Index(body, "resolvePoolReviewer(*provider")
	sideEffects := map[string]string{
		"lease":  "p.Lease(",
		"packet": "os.WriteFile(packet",
		"tab":    "herdr.TabCreate(",
	}
	if resolve < 0 {
		t.Fatal("expected the authoritative reviewer resolve in the launch path")
	}
	for name, needle := range sideEffects {
		at := strings.Index(body, needle)
		if at < 0 {
			t.Fatalf("expected %s side effect %q in the launch path", name, needle)
		}
		if resolve > at {
			t.Errorf("reviewer route must resolve before the %s side effect", name)
		}
	}
}
