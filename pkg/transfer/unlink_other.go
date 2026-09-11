//go:build !unix

package transfer

import (
	"fmt"
	"os"
)

// removeDirRelative cannot pin a parent directory on this platform; the
// conservative answer is refusal, never a path-based unlink.
func removeDirRelative(dirFile *os.File, name string) error {
	return fmt.Errorf("directory-relative unlink is unsupported on this platform")
}

// quarantineAndRemove cannot provide the rename-into-quarantine proof on
// this platform; the conservative answer is refusal, never a path-based
// unlink.
func quarantineAndRemove(dirFile *os.File, dir, name string, before os.FileInfo, pinnedDigest string) (bool, string) {
	return false, "quarantine deletion is unsupported on this platform"
}
