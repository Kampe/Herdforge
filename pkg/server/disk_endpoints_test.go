package server

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/gc"
	"github.com/Kampe/Herdforge/pkg/metrics"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

func gitRun(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
}

// diskFixture: repo on main with one clean herd/* worktree at HEAD
// (content-merged → eligible) and one dirty herd/* worktree (refused).
func diskFixture(t *testing.T) (repo, cleanWT, dirtyWT string) {
	repo = t.TempDir()
	gitRun(t, repo, "git", "init", "-b", "main")
	gitRun(t, repo, "git", "config", "user.email", "t@t")
	gitRun(t, repo, "git", "config", "user.name", "t")
	gitRun(t, repo, "git", "config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(repo, "f.txt"), []byte("base"), 0644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "git", "add", ".")
	gitRun(t, repo, "git", "commit", "-m", "initial")

	cleanWT = filepath.Join(repo, "wts", "herd-done")
	gitRun(t, repo, "git", "worktree", "add", "-b", "herd/done", cleanWT, "HEAD")
	dirtyWT = filepath.Join(repo, "wts", "herd-dirty")
	gitRun(t, repo, "git", "worktree", "add", "-b", "herd/dirty", dirtyWT, "HEAD")
	if err := os.WriteFile(filepath.Join(dirtyWT, "precious.txt"), []byte("unrecoverable"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{cleanWT, dirtyWT} {
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			if p == cleanWT {
				cleanWT = resolved
			} else {
				dirtyWT = resolved
			}
		}
	}
	return repo, cleanWT, dirtyWT
}

func newDiskServer(repo string) *ControlServer {
	s := NewControlServer("127.0.0.1:0")
	s.ControlToken = "test-capability"
	s.Metrics = metrics.NewMetricsExporter()
	s.DiskVolumes = map[string]string{"repo": repo}
	s.GC = gc.NewGCManager(repo, worktree.NewWorktreePool(repo, filepath.Join(repo, "wts")))
	s.DefaultBranch = "main"
	return s
}

func TestMetricsEndpointServesLiveDiskObservations(t *testing.T) {
	// Floors zeroed: guard reconciles to ok on the scrape's own live check.
	t.Setenv(preflight.EnvDiskMinFreeGB, "0")
	t.Setenv(preflight.EnvDiskMinFreePct, "0")
	t.Setenv(preflight.EnvDiskMinInodePct, "0")

	repo, _, _ := diskFixture(t)
	s := newDiskServer(repo)

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `herd_disk_pressure_state{state="ok"} 1`) {
		t.Fatalf("live guard state missing:\n%s", body)
	}
	if !strings.Contains(body, `herd_disk_free_bytes{volume="repo"}`) {
		t.Fatalf("live repo volume gauge missing:\n%s", body)
	}
	// The gauge is a real probe of this volume, not a seeded placeholder.
	st, err := preflight.ProbeDisk(repo)
	if err != nil || st.FreeBytes == 0 {
		t.Fatalf("probe: %v", err)
	}

	// Under an impossible floor the same scrape projects blocked.
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if !strings.Contains(rec.Body.String(), `herd_disk_pressure_state{state="blocked"} 1`) {
		t.Fatalf("blocked state not projected:\n%s", rec.Body.String())
	}
	// Restore floors so the guard recovers for later tests in this binary.
	t.Setenv(preflight.EnvDiskMinFreeGB, "0")
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
}

func TestReclamationPlanEndpointIsReadOnly(t *testing.T) {
	repo, cleanWT, dirtyWT := diskFixture(t)
	s := newDiskServer(repo)

	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, httptest.NewRequest("GET", "/v1/disk/reclamation-plan", nil))
	if rec.Code != 200 {
		t.Fatalf("plan status %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, cleanWT) || !strings.Contains(body, dirtyWT) {
		t.Fatalf("plan missing exact targets:\n%s", body)
	}
	// Read-only: both worktrees still on disk.
	for _, p := range []string{cleanWT, dirtyWT} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("plan disturbed %s: %v", p, err)
		}
	}
}


// reclaimReq builds a loopback-local, capability-bearing POST
// /v1/disk/reclaim request.
func reclaimReq(body string) *http.Request {
	req := httptest.NewRequest("POST", "/v1/disk/reclaim", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:9999"
	req.Header.Set("Authorization", "Bearer test-capability")
	return req
}

func TestReclaimEndpointAuthorization(t *testing.T) {
	repo, _, _ := diskFixture(t)
	s := newDiskServer(repo)

	// Non-loopback caller: refused outright even with the capability.
	rec := httptest.NewRecorder()
	remote := reclaimReq(`{"targets":["x"]}`)
	remote.RemoteAddr = "192.0.2.10:4444"
	s.routes().ServeHTTP(rec, remote)
	if rec.Code != 403 {
		t.Fatalf("non-loopback reclaim must 403, got %d: %s", rec.Code, rec.Body.String())
	}

	// The capability is MANDATORY: loopback without a token is 401.
	rec = httptest.NewRecorder()
	missing := reclaimReq(`{"targets":["x"]}`)
	missing.Header.Del("Authorization")
	s.routes().ServeHTTP(rec, missing)
	if rec.Code != 401 {
		t.Fatalf("missing capability must 401, got %d: %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	bad := reclaimReq(`{"targets":["x"]}`)
	bad.Header.Set("Authorization", "Bearer wrong")
	s.routes().ServeHTTP(rec, bad)
	if rec.Code != 401 {
		t.Fatalf("wrong capability must 401, got %d: %s", rec.Code, rec.Body.String())
	}
	// Correct capability + loopback proceeds to target validation (400:
	// empty target set refused by the exact-target contract, not by auth).
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, reclaimReq(`{"targets":[]}`))
	if rec.Code != 400 {
		t.Fatalf("authorized empty-target reclaim must 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestControlServerRequiresCapabilityAtStartup(t *testing.T) {
	// Unconfigured capability + mounted mutation surface: Start refuses,
	// and even direct handler calls refuse mutations (never default-open).
	repo, _, _ := diskFixture(t)
	s := newDiskServer(repo)
	s.ControlToken = "" // and no HERD_CONTROL_TOKEN in env
	t.Setenv(EnvControlToken, "")

	if err := s.Start(context.Background()); err == nil {
		_ = s.Stop(context.Background())
		t.Fatal("Start must refuse a mutation surface without a control capability")
	}
	rec := httptest.NewRecorder()
	req := reclaimReq(`{"targets":["x"]}`)
	req.Header.Del("Authorization")
	s.routes().ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("unbound capability must refuse mutations with 403, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestReclaimEndpointExactTargetsOnly(t *testing.T) {
	repo, cleanWT, dirtyWT := diskFixture(t)
	s := newDiskServer(repo)

	// Empty target set: refused — no broad cleanup mode exists.
	rec := httptest.NewRecorder()
	s.routes().ServeHTTP(rec, reclaimReq(`{"targets":[]}`))
	if rec.Code != 400 {
		t.Fatalf("empty targets must 400, got %d: %s", rec.Code, rec.Body.String())
	}

	// Exact dirty target: FAC-117 refuses it and the tree survives intact.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, reclaimReq(`{"targets":["`+dirtyWT+`"]}`))
	if rec.Code != 200 {
		t.Fatalf("dirty-target reclaim status %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"Reaped":["`+dirtyWT) {
		t.Fatalf("dirty worktree reaped: %s", rec.Body.String())
	}
	if data, err := os.ReadFile(filepath.Join(dirtyWT, "precious.txt")); err != nil || string(data) != "unrecoverable" {
		t.Fatalf("dirty content disturbed: %q err=%v", data, err)
	}

	// Exact content-merged target: reclaimed through the contract, sibling
	// dirty tree untouched.
	rec = httptest.NewRecorder()
	s.routes().ServeHTTP(rec, reclaimReq(`{"targets":["`+cleanWT+`"]}`))
	if rec.Code != 200 {
		t.Fatalf("clean-target reclaim status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(cleanWT); !os.IsNotExist(err) {
		t.Fatalf("content-merged target not reclaimed (err=%v): %s", err, rec.Body.String())
	}
	if _, err := os.Stat(dirtyWT); err != nil {
		t.Fatalf("sibling dirty worktree disturbed: %v", err)
	}
}

func TestProductionControlServerEndToEnd(t *testing.T) {
	// Compiled production constructor, real listener, real loopback HTTP —
	// not direct handler invocation.
	t.Setenv(preflight.EnvDiskMinFreeGB, "0")
	t.Setenv(preflight.EnvDiskMinFreePct, "0")
	t.Setenv(preflight.EnvDiskMinInodePct, "0")

	repo, _, dirtyWT := diskFixture(t)
	t.Setenv(EnvControlToken, "prod-capability")
	s := NewProductionControlServer("127.0.0.1:0", repo, filepath.Join(repo, "wts"), "main")
	if err := s.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer func() { _ = s.Stop(context.Background()) }()
	base := "http://" + s.BoundAddr()

	// Live metrics over real HTTP: repo/pool/temp roles present.
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`herd_disk_pressure_state{state="ok"} 1`,
		`herd_disk_free_bytes{volume="repo"}`,
		`herd_disk_free_bytes{volume="pool"}`,
		`herd_disk_free_bytes{volume="temp"}`,
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("missing %q over production HTTP:\n%s", want, body)
		}
	}

	// Authorized (real loopback + capability) reclaim of a dirty target:
	// FAC-117 JIT refusal over the wire, tree preserved.
	reclaim, _ := http.NewRequest("POST", base+"/v1/disk/reclaim", strings.NewReader(`{"targets":["`+dirtyWT+`"]}`))
	reclaim.Header.Set("Content-Type", "application/json")
	reclaim.Header.Set("Authorization", "Bearer prod-capability")
	resp, err = http.DefaultClient.Do(reclaim)
	if err != nil {
		t.Fatalf("POST reclaim: %v", err)
	}
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("reclaim status %d: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "dirty") {
		t.Fatalf("expected dirty refusal evidence over the wire: %s", body)
	}
	if _, statErr := os.Stat(dirtyWT); statErr != nil {
		t.Fatalf("dirty tree disturbed via production path: %v", statErr)
	}
	if s.ServeErr() != nil {
		t.Fatalf("runtime serve error: %v", s.ServeErr())
	}
}
