package transfer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Deterministic interleaving reproduction for the manifest path race the
// OpenAI evidence review identified (bundle-interleaving-repair-2306): the
// manifest file is replaced between path validation and the content read,
// so the bytes parsed differ from the inode whose containment and regular
// file shape were checked. Production leaves interleaveHook nil.

func TestLoadRetentionManifestRejectsReplacementBetweenValidationAndRead(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "manifests")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(dir, "retention.json")
	valid, err := json.Marshal(RetentionManifest{Version: 2, Authority: "coordinator", Bundles: []RetentionEntry{{Name: "a.bundle", Digest: strings.Repeat("a", 64), Size: 1, ModTimeUnixNano: 1}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, valid, 0o644); err != nil {
		t.Fatal(err)
	}
	replacement, err := json.Marshal(RetentionManifest{Version: 2, Authority: "coordinator", Bundles: []RetentionEntry{{Name: "b.bundle", Digest: strings.Repeat("b", 64), Size: 2, ModTimeUnixNano: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, "retention.json.replacement")
	if err := os.WriteFile(staging, replacement, 0o644); err != nil {
		t.Fatal(err)
	}
	saved := interleaveHook
	defer func() { interleaveHook = saved }()
	interleaveHook = func(stage string) {
		if stage != "manifest-pre-read" {
			return
		}
		// Inode replacement (rename over the validated path), not an
		// in-place write: the read must not consume foreign bytes.
		if err := os.Rename(staging, manifestPath); err != nil {
			t.Errorf("replacement install: %v", err)
		}
	}
	m, err := LoadRetentionManifest(root, manifestPath)
	if err == nil {
		t.Fatalf("manifest replaced between validation and read was accepted: %+v", m)
	}
}
