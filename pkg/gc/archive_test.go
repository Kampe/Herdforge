package gc

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

func TestCreateArchiveRefusedUnderDiskPressure(t *testing.T) {
	t.Setenv(preflight.EnvDiskLedgerDir, t.TempDir())
	t.Setenv(preflight.EnvDiskMinFreeGB, "1099511627776")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	err := CreateArchive(context.Background(), src, dest)
	if err == nil || !strings.Contains(err.Error(), "disk_pressure") {
		t.Fatalf("expected admission refusal, got: %v", err)
	}
	// Refused before any byte landed.
	if _, statErr := os.Stat(dest); !os.IsNotExist(statErr) {
		t.Fatal("dest exists despite refusal")
	}
	entries, _ := os.ReadDir(filepath.Dir(dest))
	if len(entries) != 0 {
		t.Fatalf("partial artifacts left: %v", entries)
	}
}

func TestCreateArchiveAdmittedWritesArchive(t *testing.T) {
	t.Setenv(preflight.EnvDiskLedgerDir, t.TempDir())
	t.Setenv(preflight.EnvDiskMinFreeGB, "0")
	t.Setenv(preflight.EnvDiskMinFreePct, "0")
	t.Setenv(preflight.EnvDiskMinInodePct, "0")
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "a.txt"), []byte("payload"), 0o644); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(t.TempDir(), "out.tar.gz")
	if err := CreateArchive(context.Background(), src, dest); err != nil {
		t.Fatalf("CreateArchive: %v", err)
	}
	f, err := os.Open(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	found := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Name == "a.txt" {
			data, _ := io.ReadAll(tr)
			if string(data) == "payload" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("archive missing expected content")
	}
}
