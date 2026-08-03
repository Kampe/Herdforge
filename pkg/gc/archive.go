package gc

// Archive creation (FAC-153): THE sanctioned archive-writing path. Archives
// are one of the unpredictable disk-growth sources named by the card, so
// creation is admission-gated with the reservation held for the entire
// write; refusal happens before a single byte lands.

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/Kampe/Herdforge/pkg/preflight"
)

// CreateArchive writes a .tar.gz of srcDir to destPath through the common
// disk admission/reservation gate. The write goes to a temp file on the
// destination volume and is renamed into place only on success; on any
// failure only the archive's own temp file is removed.
func CreateArchive(ctx context.Context, srcDir, destPath string) (retErr error) {
	release, err := preflight.AdmitDiskMutation("archive", srcDir, filepath.Dir(destPath), os.TempDir())
	if err != nil {
		return err
	}
	defer release()

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".herd-archive-*")
	if err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(tmp.Name()) // only our own partial artifact
		}
	}()

	gz := gzip.NewWriter(tmp)
	tw := tar.NewWriter(gz)
	walkErr := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		rel, err := filepath.Rel(srcDir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err := tw.Close(); walkErr == nil {
		walkErr = err
	}
	if err := gz.Close(); walkErr == nil {
		walkErr = err
	}
	if err := tmp.Close(); walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		return fmt.Errorf("archive: %w", walkErr)
	}
	if err := os.Rename(tmp.Name(), destPath); err != nil {
		return fmt.Errorf("archive: %w", err)
	}
	return nil
}
