//go:build !unix

package transfer

import "os"

// fileLinkCount cannot answer on this platform; 0 conservatively retains.
func fileLinkCount(st os.FileInfo) uint64 { return 0 }
