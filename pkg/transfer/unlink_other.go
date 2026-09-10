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
