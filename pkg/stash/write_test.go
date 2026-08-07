package stash

import (
	"os"
	"path/filepath"
)

func osWriteFile(dir, name, body string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644)
}
