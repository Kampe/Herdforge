package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
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
}

func NewControlServer(addr string) *ControlServer {
	return &ControlServer{
		Addr:      addr,
		StartTime: time.Now(),
	}
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
	resp := ServerStatusResponse{
		Status:    "healthy",
		Version:   "v0.1.0",
		UptimeSec: time.Since(s.StartTime).Seconds(),
		Timestamp: time.Now(),
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
