package server

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/gc"
	"github.com/Kampe/Herdforge/pkg/metrics"
	"github.com/Kampe/Herdforge/pkg/preflight"
	"github.com/Kampe/Herdforge/pkg/worktree"
)

type ServerStatusResponse struct {
	Status    string    `json:"status"`
	Version   string    `json:"version"`
	UptimeSec float64   `json:"uptime_sec"`
	Timestamp time.Time `json:"timestamp"`
}

type ControlServer struct {
	mu        sync.Mutex
	Addr      string
	StartTime time.Time
	httpSrv   *http.Server
	boundAddr string
	serveErr  error

	// OnServeError, when set, receives any runtime Serve failure so the
	// owning loop can surface it — a dead control plane must never look
	// healthy by silence.
	OnServeError func(error)

	// Metrics, when set, mounts /metrics with LIVE disk observation: each
	// scrape probes DiskVolumes and reads the DefaultDiskGuard projection
	// (FAC-153) — never manually seeded values.
	Metrics *metrics.MetricsExporter
	// DiskVolumes maps bounded roles (repo|pool|temp) to probe paths.
	DiskVolumes map[string]string
	// GC, when set, mounts the pressure-reclamation control path:
	// GET /v1/disk/reclamation-plan (read-only exact-target proof) and
	// POST /v1/disk/reclaim (exact targets only; broad cleanup refused).
	GC *gc.GCManager
	// DefaultBranch for reclamation classification (default "main").
	DefaultBranch string
	// ControlToken is the mandatory mutation capability (falls back to
	// HERD_CONTROL_TOKEN). It is bound to the control session when routes
	// are built; later env changes never affect a running server. This is
	// also the seam for the FAC-133 authenticated control identity — do
	// not grow bespoke identity logic here.
	ControlToken string
	sessionToken string
}

func NewControlServer(addr string) *ControlServer {
	return &ControlServer{
		Addr:      addr,
		StartTime: time.Now(),
	}
}

// NewProductionControlServer is the production constructor (FAC-153): live
// disk metrics over the repo/pool/temp volumes plus the authorized
// exact-target reclamation control path, all wired to the canonical
// worktree manager. The daemon forge loop starts this when a control
// address is configured.
func NewProductionControlServer(addr, repoRoot, worktreeDir, defaultBranch string) *ControlServer {
	// Canonicalize volume paths (symlink aliases like /var vs /private/var
	// must not split one volume into two apparent identities).
	if resolved, err := filepath.EvalSymlinks(repoRoot); err == nil {
		repoRoot = resolved
	}
	if resolved, err := filepath.EvalSymlinks(worktreeDir); err == nil {
		worktreeDir = resolved
	}
	s := NewControlServer(addr)
	s.Metrics = metrics.NewMetricsExporter()
	s.DiskVolumes = map[string]string{
		"repo": repoRoot,
		"pool": worktreeDir,
		"temp": os.TempDir(),
	}
	s.GC = gc.NewGCManager(repoRoot, worktree.NewWorktreePool(repoRoot, worktreeDir))
	s.DefaultBranch = defaultBranch
	return s
}

// routes builds the mux; extracted so tests can exercise handlers without
// binding a listener.
func (s *ControlServer) routes() *http.ServeMux {
	// Bind the mutation capability to this control session exactly once.
	s.mu.Lock()
	if s.sessionToken == "" {
		s.sessionToken = s.ControlToken
		if s.sessionToken == "" {
			s.sessionToken = os.Getenv(EnvControlToken)
		}
	}
	s.mu.Unlock()

	mux := http.NewServeMux()

	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/openapi.json", s.handleOpenAPI)
	if s.Metrics != nil {
		mux.HandleFunc("/metrics", s.handleMetrics)
	}
	if s.GC != nil {
		mux.HandleFunc("/v1/disk/reclamation-plan", s.handleReclamationPlan)
		mux.HandleFunc("/v1/disk/reclaim", s.handleReclaim)
	}
	return mux
}

func (s *ControlServer) Start(ctx context.Context) error {
	mux := s.routes()

	// A mounted mutation surface without a nonempty control capability is
	// refused at startup: default-unconfigured loopback must never be
	// destructive authority (FAC-153).
	if s.GC != nil {
		s.mu.Lock()
		tok := s.sessionToken
		s.mu.Unlock()
		if tok == "" {
			return fmt.Errorf("control capability required: set ControlToken or %s before starting a server with mutation endpoints", EnvControlToken)
		}
	}

	srv := &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}
	s.mu.Lock()
	s.httpSrv = srv
	s.mu.Unlock()

	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.Addr, err)
	}
	s.mu.Lock()
	s.boundAddr = listener.Addr().String()
	s.mu.Unlock()

	go func() {
		// A runtime Serve failure is recorded and surfaced — never
		// discarded while the process appears healthy.
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.RecordServeFailure(err)
		}
	}()

	return nil
}

// BoundAddr returns the actual listen address (useful with ":0").
func (s *ControlServer) BoundAddr() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.boundAddr
}

// RecordServeFailure records a runtime serve failure (internal Serve exit,
// tests, or an external supervisor). Consumers polling ServeErr — e.g. the
// forge loop — treat it as control-plane death and fail closed.
func (s *ControlServer) RecordServeFailure(err error) {
	s.mu.Lock()
	s.serveErr = err
	cb := s.OnServeError
	s.mu.Unlock()
	if cb != nil {
		cb(err)
	}
}

// ServeErr returns any recorded runtime Serve failure.
func (s *ControlServer) ServeErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveErr
}

func (s *ControlServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	srv := s.httpSrv
	s.mu.Unlock()
	if srv != nil {
		return srv.Shutdown(ctx)
	}
	return nil
}

// writeJSON encodes to a buffer first so an encode failure becomes an
// explicit 500 instead of a silently truncated 200 body.
func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, "encode: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(buf.Bytes())
}

func (s *ControlServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	status := "healthy"
	if err := s.ServeErr(); err != nil {
		status = "degraded: " + err.Error()
	}
	resp := ServerStatusResponse{
		Status:    status,
		Version:   "v0.1.0",
		UptimeSec: time.Since(s.StartTime).Seconds(),
		Timestamp: time.Now(),
	}
	writeJSON(w, resp)
}

func (s *ControlServer) handleOpenAPI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	openAPISpec := map[string]interface{}{
		"openapi": "3.0.0",
		"info": map[string]string{
			"title":   "Herdforge Remote Control API",
			"version": "1.0.0",
		},
		"paths": map[string]interface{}{
			"/v1/status": map[string]interface{}{
				"get": map[string]interface{}{
					"summary": "Get Herdforge daemon status",
					"responses": map[string]interface{}{
						"200": map[string]string{"description": "Healthy daemon status"},
					},
				},
			},
		},
	}
	writeJSON(w, openAPISpec)
}

// handleMetrics refreshes live disk observations (FAC-153) then delegates
// to the exporter: guard state comes from a fresh DefaultDiskGuard check
// over the configured volumes (also driving BLOCKED → recovering → ok),
// and per-role gauges come from real probes — never manually seeded.
func (s *ControlServer) handleMetrics(w http.ResponseWriter, r *http.Request) {
	paths := make([]string, 0, len(s.DiskVolumes))
	for _, p := range s.DiskVolumes {
		paths = append(paths, p)
	}
	if len(paths) > 0 {
		_ = preflight.CheckDiskPressure("metrics_scrape", paths...)
	}
	s.Metrics.SetDiskState(string(preflight.DefaultDiskGuard.State()))
	for role, p := range s.DiskVolumes {
		st, err := preflight.ProbeDisk(p)
		if err != nil {
			// An unreadable volume is surfaced explicitly — the state gauge
			// (fail-closed check above) plus a per-role unreadable gauge —
			// never a silently absent series.
			s.Metrics.SetDiskVolumeUnreadable(role)
			continue
		}
		s.Metrics.SetDiskVolume(role, st.FreeBytes, st.FreePct)
	}
	s.Metrics.Handler().ServeHTTP(w, r)
}

// handleReclamationPlan returns the read-only compiled safe-GC proof:
// exact eligible targets with per-target evidence. Nothing is removed.
func (s *ControlServer) handleReclamationPlan(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	report, err := s.GC.PressureReclamationPlan(r.Context(), s.DefaultBranch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, report)
}

type reclaimRequest struct {
	Targets []string `json:"targets"`
}

// EnvControlToken, when set, is additionally required as a Bearer token on
// mutation endpoints.
const EnvControlToken = "HERD_CONTROL_TOKEN"

func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// authorizeMutation gates destructive endpoints: safe-GC eligibility is
// necessary but NOT authorization. Callers must be loopback-local, and when
// HERD_CONTROL_TOKEN is set, present it as a Bearer token (constant-time
// compared). A server wired to a non-loopback address therefore still
// refuses remote reclamation.
func (s *ControlServer) authorizeMutation(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	tok := s.sessionToken
	s.mu.Unlock()
	// Capability is MANDATORY on every mutation: no bound capability means
	// no mutation authority at all — never default-open.
	if tok == "" {
		http.Error(w, "control capability not bound; mutation refused", http.StatusForbidden)
		return false
	}
	if !isLoopback(r.RemoteAddr) {
		http.Error(w, "mutation endpoints are loopback-only", http.StatusForbidden)
		return false
	}
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if subtle.ConstantTimeCompare([]byte(got), []byte(tok)) != 1 {
		http.Error(w, "missing or invalid control token", http.StatusUnauthorized)
		return false
	}
	return true
}

// handleReclaim executes exact-target reclamation through the FAC-117 Reap
// contract (just-in-time revalidation, salvage refs). Empty target sets are
// refused — there is no broad-cleanup mode on this endpoint.
func (s *ControlServer) handleReclaim(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeMutation(w, r) {
		return
	}
	var req reclaimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	report, err := s.GC.ReclaimExact(r.Context(), s.DefaultBranch, req.Targets)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, report)
}
