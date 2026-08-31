//go:build linux

package signerboundary

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPeerPIDOfSocketValidPaths(t *testing.T) {
	tests := []struct {
		name       string
		socketPath func(*testing.T) string
	}{
		{
			name: "filesystem ASCII",
			socketPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "signer.sock")
			},
		},
		{
			name: "filesystem high-bit byte",
			socketPath: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "signer-\x80.sock")
			},
		},
		{
			name: "abstract",
			socketPath: func(t *testing.T) string {
				return "@herd-fac-680-peer-pid"
			},
		},
		{
			name: "maximum filesystem length",
			socketPath: func(t *testing.T) string {
				dir := t.TempDir()
				const maxUnixPathBytes = 107
				nameLen := maxUnixPathBytes - len(dir) - 1
				if nameLen < 1 {
					t.Fatalf("temporary directory leaves no room for boundary socket path: len=%d", len(dir))
				}
				path := filepath.Join(dir, strings.Repeat("s", nameLen))
				if len(path) != maxUnixPathBytes {
					t.Fatalf("boundary socket path length = %d, want %d", len(path), maxUnixPathBytes)
				}
				return path
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := tt.socketPath(t)
			listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
			if err != nil {
				t.Fatalf("listen on %q: %v", path, err)
			}
			t.Cleanup(func() { _ = listener.Close() })

			if got := peerPIDOfSocket(path); got != os.Getpid() {
				t.Fatalf("peerPIDOfSocket(%q) = %d, want %d", path, got, os.Getpid())
			}
		})
	}
}

func TestPeerPIDOfSocketRefusesInvalidPaths(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "missing socket", path: filepath.Join(t.TempDir(), "missing.sock")},
		{name: "overlong socket", path: strings.Repeat("s", 108)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := peerPIDOfSocket(tt.path); got != 0 {
				t.Fatalf("peerPIDOfSocket(%q) = %d, want fail-closed zero", tt.path, got)
			}
		})
	}
}
