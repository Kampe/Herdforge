package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/metrics"
)

type ServerStatusResponse struct {
	Status     string                  `json:"status"`
	Version    string                  `json:"version"`
	UptimeSec  float64                 `json:"uptime_sec"`
	Timestamp  time.Time               `json:"timestamp"`
	Health     metrics.HealthSnapshot  `json:"health"`
	Queue      metrics.QueuePressure   `json:"queue"`
	SLO        metrics.TransitionSLO   `json:"transition_slo"`
	Signals    metrics.FleetSignals    `json:"signals"`
	Freshness  metrics.FreshnessState  `json:"freshness"`
	Conditions []metrics.ConditionCode `json:"condition_codes"`
}

type ControlServer struct {
	mu        sync.Mutex
	Addr      string
	StartTime time.Time
	httpSrv   *http.Server
	metrics   *metrics.MetricsExporter
	now       func() time.Time
	serveErr  error
}

func NewControlServer(addr string) *ControlServer {
	return &ControlServer{
		Addr:      addr,
		StartTime: time.Now(),
		metrics:   metrics.NewMetricsExporter(),
		now:       time.Now,
	}
}

func NewControlServerWithMetrics(addr string, exporter *metrics.MetricsExporter, now func() time.Time) *ControlServer {
	if exporter == nil {
		exporter = metrics.NewMetricsExporter()
	}
	if now == nil {
		now = time.Now
	}
	return &ControlServer{Addr: addr, StartTime: now(), metrics: exporter, now: now}
}

func (s *ControlServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/openapi.json", s.handleOpenAPI)
	mux.Handle("/metrics", s.metrics.Handler())

	httpSrv := &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}
	s.mu.Lock()
	s.httpSrv = httpSrv
	s.mu.Unlock()

	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.Addr, err)
	}

	go func() {
		if err := httpSrv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.mu.Lock()
			s.serveErr = err
			s.mu.Unlock()
		}
	}()

	return nil
}

// ServeError exposes asynchronous listener failures without inventing a
// successful server state. It is nil until Serve reports an error.
func (s *ControlServer) ServeError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveErr
}

func (s *ControlServer) Stop(ctx context.Context) error {
	s.mu.Lock()
	httpSrv := s.httpSrv
	s.mu.Unlock()
	if httpSrv != nil {
		return httpSrv.Shutdown(ctx)
	}
	return nil
}

func (s *ControlServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	now := s.now()
	view := s.metrics.ReadAt(now)
	health, queue, slo, signals := view.Health, view.Queue, view.SLO, view.Signals
	status := "healthy"
	if !health.Liveness {
		status = "unhealthy"
	} else if !view.Freshness.Ready {
		status = "degraded"
	}
	resp := ServerStatusResponse{
		Status:     status,
		Version:    "v0.1.0",
		UptimeSec:  now.Sub(s.StartTime).Seconds(),
		Timestamp:  now,
		Health:     health,
		Queue:      queue,
		SLO:        slo,
		Signals:    signals,
		Freshness:  view.Freshness,
		Conditions: view.Conditions,
	}
	payload, err := json.Marshal(resp)
	if err != nil {
		http.Error(w, "status encoding failed", http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(payload); err != nil {
		s.mu.Lock()
		s.serveErr = fmt.Errorf("status response write: %w", err)
		s.mu.Unlock()
	}
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
					"summary": "Get liveness, readiness, freshness, and bounded fleet conditions",
					"responses": map[string]interface{}{
						"200": map[string]string{"description": "Status is served even when readiness is degraded"},
					},
					"x-bounded-condition-codes": []string{"stalled_work", "dropped_callback", "review_saturation", "dead_provider", "integration_backlog", "dead_letters", "eligible_idle"},
				},
			},
			"/metrics": map[string]interface{}{
				"get": map[string]interface{}{
					"summary":   "Prometheus metrics with bounded labels and freshness gates",
					"responses": map[string]string{"200": "Metrics text", "500": "Encoding or serving failure"},
				},
			},
		},
	}
	payload, err := json.Marshal(openAPISpec)
	if err != nil {
		http.Error(w, "openapi encoding failed", http.StatusInternalServerError)
		return
	}
	if _, err := w.Write(payload); err != nil {
		s.mu.Lock()
		s.serveErr = fmt.Errorf("openapi response write: %w", err)
		s.mu.Unlock()
	}
}
