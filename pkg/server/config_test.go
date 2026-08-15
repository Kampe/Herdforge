package server

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestControlServer_ConfigDefaults(t *testing.T) {
	cfg := DefaultConfig("127.0.0.1:0")
	if cfg.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Fatalf("expected ReadHeaderTimeout %v, got %v", DefaultReadHeaderTimeout, cfg.ReadHeaderTimeout)
	}
	if cfg.ReadTimeout != DefaultReadTimeout {
		t.Fatalf("expected ReadTimeout %v, got %v", DefaultReadTimeout, cfg.ReadTimeout)
	}
	if cfg.WriteTimeout != DefaultWriteTimeout {
		t.Fatalf("expected WriteTimeout %v, got %v", DefaultWriteTimeout, cfg.WriteTimeout)
	}
	if cfg.IdleTimeout != DefaultIdleTimeout {
		t.Fatalf("expected IdleTimeout %v, got %v", DefaultIdleTimeout, cfg.IdleTimeout)
	}
	if cfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Fatalf("expected ShutdownTimeout %v, got %v", DefaultShutdownTimeout, cfg.ShutdownTimeout)
	}

	srv, err := NewControlServerWithConfig(Config{Addr: "127.0.0.1:0"})
	if err != nil {
		t.Fatalf("unexpected error with zero-value timeouts: %v", err)
	}
	activeCfg := srv.Config()
	if activeCfg.ReadHeaderTimeout != DefaultReadHeaderTimeout {
		t.Errorf("expected default ReadHeaderTimeout, got %v", activeCfg.ReadHeaderTimeout)
	}
	if activeCfg.ReadTimeout != DefaultReadTimeout {
		t.Errorf("expected default ReadTimeout, got %v", activeCfg.ReadTimeout)
	}
	if activeCfg.WriteTimeout != DefaultWriteTimeout {
		t.Errorf("expected default WriteTimeout, got %v", activeCfg.WriteTimeout)
	}
	if activeCfg.IdleTimeout != DefaultIdleTimeout {
		t.Errorf("expected default IdleTimeout, got %v", activeCfg.IdleTimeout)
	}
	if activeCfg.ShutdownTimeout != DefaultShutdownTimeout {
		t.Errorf("expected default ShutdownTimeout, got %v", activeCfg.ShutdownTimeout)
	}
}

func TestControlServer_ConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		cfg         Config
		wantErr     bool
		errContains string
	}{
		{
			name: "valid custom configuration",
			cfg: Config{
				Addr:              "127.0.0.1:0",
				ReadHeaderTimeout: 2 * time.Second,
				ReadTimeout:       5 * time.Second,
				WriteTimeout:      5 * time.Second,
				IdleTimeout:       30 * time.Second,
				ShutdownTimeout:   3 * time.Second,
			},
			wantErr: false,
		},
		{
			name: "negative ReadHeaderTimeout",
			cfg: Config{
				ReadHeaderTimeout: -1 * time.Second,
			},
			wantErr:     true,
			errContains: "read header timeout cannot be negative",
		},
		{
			name: "ReadHeaderTimeout below minimum",
			cfg: Config{
				ReadHeaderTimeout: 1 * time.Millisecond,
			},
			wantErr:     true,
			errContains: "below minimum",
		},
		{
			name: "ReadHeaderTimeout exceeds maximum",
			cfg: Config{
				ReadHeaderTimeout: 2 * time.Minute,
			},
			wantErr:     true,
			errContains: "exceeds maximum",
		},
		{
			name: "negative ReadTimeout",
			cfg: Config{
				ReadTimeout: -1 * time.Second,
			},
			wantErr:     true,
			errContains: "read timeout cannot be negative",
		},
		{
			name: "ReadTimeout below minimum",
			cfg: Config{
				ReadTimeout: 1 * time.Millisecond,
			},
			wantErr:     true,
			errContains: "below minimum",
		},
		{
			name: "ReadTimeout exceeds maximum",
			cfg: Config{
				ReadTimeout: 10 * time.Minute,
			},
			wantErr:     true,
			errContains: "exceeds maximum",
		},
		{
			name: "negative WriteTimeout",
			cfg: Config{
				WriteTimeout: -1 * time.Second,
			},
			wantErr:     true,
			errContains: "write timeout cannot be negative",
		},
		{
			name: "WriteTimeout below minimum",
			cfg: Config{
				WriteTimeout: 1 * time.Millisecond,
			},
			wantErr:     true,
			errContains: "below minimum",
		},
		{
			name: "WriteTimeout exceeds maximum",
			cfg: Config{
				WriteTimeout: 10 * time.Minute,
			},
			wantErr:     true,
			errContains: "exceeds maximum",
		},
		{
			name: "negative IdleTimeout",
			cfg: Config{
				IdleTimeout: -1 * time.Second,
			},
			wantErr:     true,
			errContains: "idle timeout cannot be negative",
		},
		{
			name: "IdleTimeout below minimum",
			cfg: Config{
				IdleTimeout: 1 * time.Millisecond,
			},
			wantErr:     true,
			errContains: "below minimum",
		},
		{
			name: "IdleTimeout exceeds maximum",
			cfg: Config{
				IdleTimeout: 30 * time.Minute,
			},
			wantErr:     true,
			errContains: "exceeds maximum",
		},
		{
			name: "negative ShutdownTimeout",
			cfg: Config{
				ShutdownTimeout: -1 * time.Second,
			},
			wantErr:     true,
			errContains: "shutdown timeout cannot be negative",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr && tt.errContains != "" {
				if err == nil || !strings.Contains(err.Error(), tt.errContains) {
					t.Fatalf("expected error containing %q, got %v", tt.errContains, err)
				}
			}

			_, newErr := NewControlServerWithConfig(tt.cfg)
			if (newErr != nil) != tt.wantErr {
				t.Fatalf("NewControlServerWithConfig() error = %v, wantErr %v", newErr, tt.wantErr)
			}
		})
	}
}

func TestControlServer_HTTPServerTimeoutsApplied(t *testing.T) {
	customCfg := Config{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 50 * time.Millisecond,
		ReadTimeout:       100 * time.Millisecond,
		WriteTimeout:      200 * time.Millisecond,
		IdleTimeout:       300 * time.Millisecond,
	}

	srv, err := NewControlServerWithConfig(customCfg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(ctx)

	httpSrv := srv.HTTPServer()
	if httpSrv == nil {
		t.Fatal("expected httpSrv to be initialized after Start")
	}

	if httpSrv.ReadHeaderTimeout != customCfg.ReadHeaderTimeout {
		t.Errorf("expected ReadHeaderTimeout %v, got %v", customCfg.ReadHeaderTimeout, httpSrv.ReadHeaderTimeout)
	}
	if httpSrv.ReadTimeout != customCfg.ReadTimeout {
		t.Errorf("expected ReadTimeout %v, got %v", customCfg.ReadTimeout, httpSrv.ReadTimeout)
	}
	if httpSrv.WriteTimeout != customCfg.WriteTimeout {
		t.Errorf("expected WriteTimeout %v, got %v", customCfg.WriteTimeout, httpSrv.WriteTimeout)
	}
	if httpSrv.IdleTimeout != customCfg.IdleTimeout {
		t.Errorf("expected IdleTimeout %v, got %v", customCfg.IdleTimeout, httpSrv.IdleTimeout)
	}
}
