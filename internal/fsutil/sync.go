// Package fsutil provides filesystem operations shared by persistent stores.
package fsutil

import (
	"os"
	"runtime"
)

// SyncDir persists directory entries where the OS supports directory fsync.
// Windows does not support flushing a directory opened by os.Open. Callers
// still sync file contents before publishing them; directory metadata durability
// on Windows is provided by the filesystem rather than an explicit flush.
func SyncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
