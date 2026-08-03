package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/Kampe/Herdforge/pkg/metrics"
)

type ServerStatusResponse struct {
	Status    string                 `json:"status"`
	Version   string                 `json:"version"`
	UptimeSec float64                `json:"uptime_sec"`
	Timestamp time.Time              `json:"timestamp"`
	Health    metrics.HealthSnapshot `json:"health"`
	Queue     metrics.QueuePressure  `json:"queue"`
	SLO       metrics.TransitionSLO  `json:"transition_slo"`
}

type ControlServer struct {
	mu        sync.Mutex
	Addr      string
	StartTime time.Time
	httpSrv   *http.Server
	metrics   *metrics.MetricsExporter
	now       func() time.Time
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

	s.httpSrv = &http.Server{
		Addr:    s.Addr,
		Handler: mux,
	}

	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.Addr, err)
	}

	go func() {
		_ = s.httpSrv.Serve(listener)
	}()

	return nil
}

func (s *ControlServer) Stop(ctx context.Context) error {
	if s.httpSrv != nil {
		return s.httpSrv.Shutdown(ctx)
	}
	return nil
}

func (s *ControlServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	health, queue, slo := s.metrics.Snapshot()
	status := "healthy"
	if !health.Readiness || !queue.Known || queue.Error != "" {
		status = "degraded"
	}
	resp := ServerStatusResponse{
		Status:    status,
		Version:   "v0.1.0",
		UptimeSec: s.now().Sub(s.StartTime).Seconds(),
		Timestamp: s.now(),
		Health:    health,
		Queue:     queue,
		SLO:       slo,
	}
	_ = json.NewEncoder(w).Encode(resp)
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
	_ = json.NewEncoder(w).Encode(openAPISpec)
}
