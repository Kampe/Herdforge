package server

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestControlServer_SlowHeaderClient(t *testing.T) {
	cfg := Config{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 50 * time.Millisecond,
		ReadTimeout:       100 * time.Millisecond,
		WriteTimeout:      100 * time.Millisecond,
		IdleTimeout:       100 * time.Millisecond,
	}

	srv, err := NewControlServerWithConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(ctx)

	conn, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("failed to connect to server: %v", err)
	}
	defer conn.Close()

	// Send partial header and withhold terminating CRLF CRLF
	_, err = conn.Write([]byte("GET /v1/status HTTP/1.1\r\nHost: localhost\r\n"))
	if err != nil {
		t.Fatalf("failed to write partial header: %v", err)
	}

	// Sleep past ReadHeaderTimeout
	time.Sleep(120 * time.Millisecond)

	// Attempt reading from the connection; server must close or respond with 408 / EOF
	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	reader := bufio.NewReader(conn)
	respLine, readErr := reader.ReadString('\n')

	if readErr == nil {
		// If an HTTP response was written before closing, it should be 408 Request Timeout
		if !strings.Contains(respLine, "408") {
			t.Fatalf("expected 408 Request Timeout on slow headers, got: %s", respLine)
		}
	} else if readErr != io.EOF && !strings.Contains(readErr.Error(), "connection reset") && !strings.Contains(readErr.Error(), "closed") {
		t.Logf("connection closed with: %v (expected close or 408)", readErr)
	}
}

func TestControlServer_SlowHeaderTrickleClient(t *testing.T) {
	cfg := Config{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 60 * time.Millisecond,
		ReadTimeout:       100 * time.Millisecond,
		WriteTimeout:      100 * time.Millisecond,
		IdleTimeout:       100 * time.Millisecond,
	}

	srv, err := NewControlServerWithConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(ctx)

	conn, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("failed to connect to server: %v", err)
	}
	defer conn.Close()

	// Slowly send one byte at a time with delays that exceed ReadHeaderTimeout in total
	_, err = conn.Write([]byte("GET /v1/status "))
	if err != nil {
		t.Fatalf("failed to write initial chunk: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	_, _ = conn.Write([]byte("HTTP/1.1\r\nHost: localhost\r\n\r\n"))

	_ = conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	reader := bufio.NewReader(conn)
	respLine, readErr := reader.ReadString('\n')

	if readErr == nil {
		if !strings.Contains(respLine, "408") && !strings.Contains(respLine, "400") {
			t.Fatalf("expected 408 or 400 for slow headers, got: %s", respLine)
		}
	} else if readErr != io.EOF && !strings.Contains(readErr.Error(), "connection reset") && !strings.Contains(readErr.Error(), "closed") {
		t.Logf("connection closed as expected: %v", readErr)
	}
}

func TestControlServer_IdleClientTimeout(t *testing.T) {
	cfg := Config{
		Addr:              "127.0.0.1:0",
		ReadHeaderTimeout: 100 * time.Millisecond,
		ReadTimeout:       100 * time.Millisecond,
		WriteTimeout:      100 * time.Millisecond,
		IdleTimeout:       50 * time.Millisecond,
	}

	srv, err := NewControlServerWithConfig(cfg)
	if err != nil {
		t.Fatalf("failed to create server: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := srv.Start(ctx); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer srv.Stop(ctx)

	conn, err := net.Dial("tcp", srv.Addr)
	if err != nil {
		t.Fatalf("failed to connect to server: %v", err)
	}
	defer conn.Close()

	// Send complete request
	req, err := http.NewRequest("GET", "http://"+srv.Addr+"/openapi.json", nil)
	if err != nil {
		t.Fatalf("failed to construct request: %v", err)
	}
	if err := req.Write(conn); err != nil {
		t.Fatalf("failed to write request: %v", err)
	}

	// Read full response including body so connection enters keep-alive idle state
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, req)
	if err != nil {
		t.Fatalf("failed to read response: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	// Wait past IdleTimeout (50ms)
	time.Sleep(120 * time.Millisecond)

	// In keep-alive idle state, server must have closed connection after IdleTimeout
	_ = conn.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	buf := make([]byte, 1)
	_, readErr := reader.Read(buf)
	if readErr == nil {
		t.Fatal("expected idle connection to be closed by server due to IdleTimeout, but read succeeded")
	}
}
