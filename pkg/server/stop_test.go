package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestServer_Stop_NilHTTPSrv(t *testing.T) {
	srv := NewControlServer("127.0.0.1:18999")
	// Without Start(), httpSrv is nil — Stop should return nil
	err := srv.Stop(context.Background())
	if err != nil {
		t.Errorf("expected nil error when httpSrv is nil, got: %v", err)
	}
}

func TestServer_StartStop(t *testing.T) {
	srv := NewControlServer("127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() {
		if err := srv.Start(ctx); err != nil && err.Error() != "http: Server closed" {
			t.Logf("start returned: %v", err)
		}
	}()
	time.Sleep(200 * time.Millisecond)

	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("expected stop success, got err: %v", err)
	}
}

func TestServer_Start_BindError(t *testing.T) {
	ctx := context.Background()
	// Privileged port 1 to ensure error binding.
	srv := NewControlServer("127.0.0.1:1")
	if err := srv.Start(ctx); err == nil {
		srv.Stop(ctx)
		t.Fatal("expected error binding to privileged port")
	}
}

func TestServer_Stop_NilContextAppliesDefaultTimeout(t *testing.T) {
	srv := NewControlServer("127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	// Passing nil context to Stop should use default shutdown timeout and gracefully stop.
	if err := srv.Stop(nil); err != nil {
		t.Fatalf("expected graceful stop with nil context, got err: %v", err)
	}
}

func TestServer_GracefulShutdownInFlight(t *testing.T) {
	cfg := Config{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 1 * time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       2 * time.Second,
		ShutdownTimeout:   2 * time.Second,
	}

	srv, err := NewControlServerWithConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	startCtx, startCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer startCancel()

	if err := srv.Start(startCtx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)

	clientDone := make(chan struct{})
	var respStatusCode int

	go func() {
		defer wg.Done()
		resp, err := http.Get("http://" + srv.Addr + "/v1/status")
		if err == nil {
			respStatusCode = resp.StatusCode
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
		close(clientDone)
	}()

	<-clientDone

	if respStatusCode != http.StatusOK {
		t.Errorf("expected 200 OK for in-flight request, got %d", respStatusCode)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer stopCancel()

	if err := srv.Stop(stopCtx); err != nil {
		t.Fatalf("expected graceful shutdown, got %v", err)
	}

	wg.Wait()
}

func TestServer_ShutdownDeadlineExceeded(t *testing.T) {
	srv := NewControlServer("127.0.0.1:0")
	startCtx, startCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer startCancel()

	if err := srv.Start(startCtx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	// Keep an active TCP connection open so Shutdown waits for drain
	conn, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("failed to dial server: %v", err)
	}
	defer conn.Close()

	// Create an already-cancelled context
	stopCtx, stopCancel := context.WithCancel(context.Background())
	stopCancel()

	err = srv.Stop(stopCtx)
	if err == nil {
		t.Fatal("expected error shutting down with cancelled context and active connection, got nil")
	}
}
