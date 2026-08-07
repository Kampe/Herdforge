package signerboundary

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// atomicWriteFile writes data via temp+fsync+rename+dir fsync (crash-safe).
func atomicWriteFile(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	return syncDir(dir)
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open dir for fsync: %w", err)
	}
	defer d.Close()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("dir fsync: %w", err)
	}
	return nil
}

func atomicWriteJSON(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(path, data, mode)
}

// secureRemove overwrites then removes; returns error if the path still exists
// or remains readable afterward (FAC-169 §8 — never report revoked while usable).
func secureRemove(path string) error {
	fi, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if fi.Mode().IsRegular() {
		zeros := make([]byte, fi.Size())
		if werr := os.WriteFile(path, zeros, 0o600); werr != nil {
			// Still attempt remove
			_ = werr
		}
		_ = syncDir(filepath.Dir(path))
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("%w: path still present after revoke: %s", ErrRevoked, path)
	}
	return nil
}
