package transfer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A second concurrent origin writer inside the mismatch-to-restore window
// must survive a quarantine restore: the restore is an atomic no-clobber
// operation, never a plain rename over an occupied origin name. Both the
// quarantined object and the second writer's file must survive.
func TestQuarantineRestoreNoClobberSecondWriterSurvives(t *testing.T) {
	dir := t.TempDir()
	dirFile, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dirFile.Close()

	name := "object"
	original := []byte("original-A")
	replacement := []byte("replacement-B")
	secondWriter := []byte("second-writer-C")

	writeAt := func(content []byte) os.FileInfo {
		// Always a fresh inode: replace-by-rename semantics, never an
		// in-place truncate (which would keep the same inode and defeat
		// the mismatch setup).
		_ = os.Remove(filepath.Join(dir, name))
		if wErr := os.WriteFile(filepath.Join(dir, name), content, 0o644); wErr != nil {
			t.Fatal(wErr)
		}
		st, stErr := os.Lstat(filepath.Join(dir, name))
		if stErr != nil {
			t.Fatal(stErr)
		}
		return st
	}

	before := writeAt(original)
	// Pin the original inode with a live descriptor. writeAt replaces by
	// unlink-and-recreate, and filesystems that recycle freed inode
	// numbers (ext4, overlayfs on Linux CI) may hand the replacement the
	// original's inode number. os.SameFile compares only device and inode
	// number, so without a live pin the stale snapshot `before` and the
	// quarantined replacement can compare equal across generations and
	// divert the restore into the content-identity branch (which still
	// preserves the object, but reports a different reason). Holding the
	// descriptor keeps the original inode allocated until the test ends,
	// making the two generations provably distinct on every filesystem,
	// exactly like production's revalidation of a live file.
	beforeFile, statErr := os.Open(filepath.Join(dir, name))
	if statErr != nil {
		t.Fatal(statErr)
	}
	defer beforeFile.Close()
	if beforeStat, statErr := beforeFile.Stat(); statErr != nil {
		t.Fatal(statErr)
	} else {
		before = beforeStat
	}
	// Deterministic pre-rename swap: the entry is replaced between the
	// final revalidation (before) and the quarantine rename, so the
	// quarantine receives the replacement and the mismatch path restores.
	_ = writeAt(replacement)

	inWindow := make(chan struct{})
	writerDone := make(chan struct{})
	saved := interleaveHook
	interleaveHook = func(stage string) {
		if stage == "quarantined" {
			close(inWindow)
			// The window stays open until the second writer's file is
			// actually on disk: the restore must not proceed first.
			<-writerDone
		}
	}
	t.Cleanup(func() { interleaveHook = saved })

	done := make(chan string, 1)
	go func() {
		_, reason := quarantineAndRemove(dirFile, dir, name, before, "sha256:pinned-digest")
		done <- reason
	}()

	select {
	case <-inWindow:
	case <-time.After(10 * time.Second):
		interleaveHook = nil
		t.Fatal("old candidate: no deterministic mismatch-to-restore window (quarantined hook never fired); restore is a plain clobbering rename")
	}
	// The second concurrent origin writer creates a new file at the origin
	// name inside the window.
	if wErr := os.WriteFile(filepath.Join(dir, name), secondWriter, 0o644); wErr != nil {
		t.Fatal(wErr)
	}
	close(writerDone)

	reason := <-done
	if strings.Contains(reason, "-failed") {
		t.Fatalf("restore reported failure: %s", reason)
	}
	if !strings.Contains(reason, "restored-after-quarantine") {
		t.Fatalf("mismatch path not taken: %s", reason)
	}

	// The second writer's file must be INTACT at the origin name.
	kept, readErr := os.ReadFile(filepath.Join(dir, name))
	if readErr != nil {
		t.Fatalf("second writer's file deleted by restore: %v", readErr)
	}
	if string(kept) != string(secondWriter) {
		t.Fatalf("second writer's file was overwritten by the restore: %q", kept)
	}
	// The quarantined replacement must also survive, at the reported
	// recovery path or in quarantine. Reported paths are relative to the
	// pinned parent directory.
	recovered := strings.TrimPrefix(reason, "restored-after-quarantine: foreign replacement preserved at ")
	if recovered == reason {
		t.Fatalf("recovery path not reported: %s", reason)
	}
	preserved, readErr := os.ReadFile(filepath.Join(dir, recovered))
	if readErr != nil {
		t.Fatalf("quarantined object lost: %v (reason=%s)", readErr, reason)
	}
	if string(preserved) != string(replacement) {
		t.Fatalf("quarantined object corrupted: %q", preserved)
	}
}

// With the origin name still free, the no-clobber restore restores to the
// origin name exactly as the plain rename did.
func TestQuarantineRestoreToFreeOriginName(t *testing.T) {
	dir := t.TempDir()
	dirFile, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer dirFile.Close()

	name := "object"
	original := []byte("original-A")
	replacement := []byte("replacement-B")
	if err := os.WriteFile(filepath.Join(dir, name), original, 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), replacement, 0o644); err != nil {
		t.Fatal(err)
	}

	_, reason := quarantineAndRemove(dirFile, dir, name, before, "sha256:pinned-digest")
	if !strings.Contains(reason, "restored-after-quarantine") || strings.Contains(reason, "-failed") {
		t.Fatalf("mismatch path not taken cleanly: %s", reason)
	}
	restored, readErr := os.ReadFile(filepath.Join(dir, name))
	if readErr != nil {
		t.Fatalf("restored object missing at origin name: %v (reason=%s)", readErr, reason)
	}
	if string(restored) != string(replacement) {
		t.Fatalf("restored object corrupted: %q", restored)
	}
}
