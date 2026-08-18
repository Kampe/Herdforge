package server

import (
	"context"
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

	"github.com/Kampe/Herdforge/pkg/metrics"
	"github.com/Kampe/Herdforge/pkg/webhook"
)

const WebhookPath = "/v1/webhook"

const (
	DefaultReadHeaderTimeout = 5 * time.Second
	DefaultReadTimeout       = 10 * time.Second
	DefaultWriteTimeout      = 10 * time.Second
	DefaultIdleTimeout       = 60 * time.Second
	DefaultShutdownTimeout   = 5 * time.Second

	MinReadHeaderTimeout = 10 * time.Millisecond
	MaxReadHeaderTimeout = 60 * time.Second

	MinReadTimeout = 10 * time.Millisecond
	MaxReadTimeout = 5 * time.Minute

	MinWriteTimeout = 10 * time.Millisecond
	MaxWriteTimeout = 5 * time.Minute

	MinIdleTimeout = 10 * time.Millisecond
	MaxIdleTimeout = 15 * time.Minute
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

// Config defines the configuration for ControlServer including bounded timeouts.
type Config struct {
	Addr              string
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	ShutdownTimeout   time.Duration
	Metrics           *metrics.MetricsExporter
	Now               func() time.Time
	WebhookEnabled    bool
	WebhookSecret     string
	WebhookStorePath  string
	WebhookConfig     *webhook.Config
}

// DefaultConfig returns a Config populated with default timeouts.
func DefaultConfig(addr string) Config {
	return Config{
		Addr:              addr,
		ReadHeaderTimeout: DefaultReadHeaderTimeout,
		ReadTimeout:       DefaultReadTimeout,
		WriteTimeout:      DefaultWriteTimeout,
		IdleTimeout:       DefaultIdleTimeout,
		ShutdownTimeout:   DefaultShutdownTimeout,
		Now:               time.Now,
	}
}

// Validate ensures that configured timeouts are within safe and bounded limits.
func (c Config) Validate() error {
	if c.ReadHeaderTimeout < 0 {
		return fmt.Errorf("read header timeout cannot be negative: %v", c.ReadHeaderTimeout)
	}
	if c.ReadHeaderTimeout > 0 && c.ReadHeaderTimeout < MinReadHeaderTimeout {
		return fmt.Errorf("read header timeout %v is below minimum %v", c.ReadHeaderTimeout, MinReadHeaderTimeout)
	}
	if c.ReadHeaderTimeout > MaxReadHeaderTimeout {
		return fmt.Errorf("read header timeout %v exceeds maximum %v", c.ReadHeaderTimeout, MaxReadHeaderTimeout)
	}

	if c.ReadTimeout < 0 {
		return fmt.Errorf("read timeout cannot be negative: %v", c.ReadTimeout)
	}
	if c.ReadTimeout > 0 && c.ReadTimeout < MinReadTimeout {
		return fmt.Errorf("read timeout %v is below minimum %v", c.ReadTimeout, MinReadTimeout)
	}
	if c.ReadTimeout > MaxReadTimeout {
		return fmt.Errorf("read timeout %v exceeds maximum %v", c.ReadTimeout, MaxReadTimeout)
	}

	if c.WriteTimeout < 0 {
		return fmt.Errorf("write timeout cannot be negative: %v", c.WriteTimeout)
	}
	if c.WriteTimeout > 0 && c.WriteTimeout < MinWriteTimeout {
		return fmt.Errorf("write timeout %v is below minimum %v", c.WriteTimeout, MinWriteTimeout)
	}
	if c.WriteTimeout > MaxWriteTimeout {
		return fmt.Errorf("write timeout %v exceeds maximum %v", c.WriteTimeout, MaxWriteTimeout)
	}

	if c.IdleTimeout < 0 {
		return fmt.Errorf("idle timeout cannot be negative: %v", c.IdleTimeout)
	}
	if c.IdleTimeout > 0 && c.IdleTimeout < MinIdleTimeout {
		return fmt.Errorf("idle timeout %v is below minimum %v", c.IdleTimeout, MinIdleTimeout)
	}
	if c.IdleTimeout > MaxIdleTimeout {
		return fmt.Errorf("idle timeout %v exceeds maximum %v", c.IdleTimeout, MaxIdleTimeout)
	}

	if c.ShutdownTimeout < 0 {
		return fmt.Errorf("shutdown timeout cannot be negative: %v", c.ShutdownTimeout)
	}

	return nil
}

type ControlServer struct {
	mu        sync.Mutex
	Addr      string
	StartTime time.Time
	httpSrv   *http.Server
	metrics   *metrics.MetricsExporter
	now       func() time.Time
	serveErr  error
	config    Config
	webhook   *webhook.Receiver
	webhookDB *webhook.Store
	closeOnce sync.Once
	closeErr  error
}

// NewControlServer initializes a ControlServer with standard default bounded timeouts.
func NewControlServer(addr string) *ControlServer {
	return NewControlServerWithMetrics(addr, nil, nil)
}

// NewControlServerWithMetrics initializes a ControlServer with exporter and clock.
func NewControlServerWithMetrics(addr string, exporter *metrics.MetricsExporter, now func() time.Time) *ControlServer {
	cfg := DefaultConfig(addr)
	cfg.Metrics = exporter
	if now != nil {
		cfg.Now = now
	}
	srv, _ := NewControlServerWithConfig(cfg)
	return srv
}

// NewControlServerWithConfig initializes and validates a ControlServer with the provided Config.
func NewControlServerWithConfig(cfg Config) (*ControlServer, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.ReadHeaderTimeout == 0 {
		cfg.ReadHeaderTimeout = DefaultReadHeaderTimeout
	}
	if cfg.ReadTimeout == 0 {
		cfg.ReadTimeout = DefaultReadTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = DefaultShutdownTimeout
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.NewMetricsExporter()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}

	var receiver *webhook.Receiver
	var store *webhook.Store
	if cfg.WebhookEnabled {
		secret := strings.TrimSpace(cfg.WebhookSecret)
		if secret == "" {
			secret = strings.TrimSpace(os.Getenv("HERD_WEBHOOK_SECRET"))
		}
		if secret == "" {
			return nil, errors.New("server: webhook secret is required when webhook is enabled (fail-closed)")
		}
		storePath := strings.TrimSpace(cfg.WebhookStorePath)
		if storePath == "" {
			storePath = strings.TrimSpace(os.Getenv("HERD_WEBHOOK_STORE_PATH"))
		}
		if storePath == "" {
			storePath = filepath.Join(".", ".herd", "webhook.db")
		}
		var err error
		store, err = webhook.NewStore(storePath)
		if err != nil {
			return nil, fmt.Errorf("server: open webhook store: %w", err)
		}
		receiver, err = webhook.NewReceiver(secret, store, cfg.WebhookConfig)
		if err != nil {
			_ = store.Close()
			return nil, fmt.Errorf("server: configure webhook receiver: %w", err)
		}
		receiver.RegisterHandler(func(*webhook.WebhookEvent) error { return nil })
	}

	return &ControlServer{
		Addr:      cfg.Addr,
		StartTime: cfg.Now(),
		metrics:   cfg.Metrics,
		now:       cfg.Now,
		config:    cfg,
		webhook:   receiver,
		webhookDB: store,
	}, nil
}

// Config returns a copy of the ControlServer configuration.
func (s *ControlServer) Config() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.config
}

// HTTPServer returns the active http.Server instance if running, or nil.
func (s *ControlServer) HTTPServer() *http.Server {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.httpSrv
}

func (s *ControlServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/openapi.json", s.handleOpenAPI)
	mux.Handle("/metrics", s.metrics.Handler())
	if s.webhook != nil {
		mux.Handle(WebhookPath, s.webhook)
	}

	s.mu.Lock()
	cfg := s.config
	s.mu.Unlock()

	httpSrv := &http.Server{
		Addr:              s.Addr,
		Handler:           mux,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}
	s.mu.Lock()
	s.httpSrv = httpSrv
	s.mu.Unlock()

	listener, err := net.Listen("tcp", s.Addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", s.Addr, err)
	}

	s.mu.Lock()
	s.Addr = listener.Addr().String()
	s.config.Addr = s.Addr
	s.mu.Unlock()

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
	cfg := s.config
	s.mu.Unlock()
	if httpSrv != nil {
		if ctx == nil {
			var cancel context.CancelFunc
			timeout := cfg.ShutdownTimeout
			if timeout == 0 {
				timeout = DefaultShutdownTimeout
			}
			ctx, cancel = context.WithTimeout(context.Background(), timeout)
			defer cancel()
		}
		if err := ctx.Err(); err != nil {
			_ = httpSrv.Close()
			return err
		}
		err := httpSrv.Shutdown(ctx)
		return s.closeWebhookStore(err)
	}
	return s.closeWebhookStore(nil)
}

func (s *ControlServer) closeWebhookStore(serverErr error) error {
	s.closeOnce.Do(func() {
		if s.webhookDB != nil {
			s.closeErr = s.webhookDB.Close()
		}
	})
	if serverErr != nil {
		return serverErr
	}
	return s.closeErr
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
	if s.webhook != nil {
		openAPISpec["paths"].(map[string]interface{})[WebhookPath] = map[string]interface{}{
			"post": map[string]interface{}{
				"summary":   "Receive authenticated and durably persisted provider deliveries",
				"responses": map[string]string{"200": "Delivery accepted", "401": "Invalid or missing signature", "413": "Payload too large"},
			},
		}
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
