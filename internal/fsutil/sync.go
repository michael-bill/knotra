// Package fsutil provides filesystem operations shared by persistent stores.
package fsutil

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
)

// OpenPrivateRoot persists newly created parent entries before accepting files.
// The confined handle also keeps publication on the same directory if its path
// is renamed concurrently. Shared filesystems must support these OS operations.
func OpenPrivateRoot(directory string) (*os.Root, error) {
	absolute, err := filepath.Abs(directory)
	if err != nil {
		return nil, err
	}
	parents := []string{filepath.Dir(absolute)}
	for parent := filepath.Dir(absolute); parent != filepath.Dir(parent); parent = filepath.Dir(parent) {
		if _, err := os.Lstat(parent); err == nil {
			break
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		parents = append(parents, filepath.Dir(parent))
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("private storage directory must be a real directory")
	}
	for _, path := range parents {
		if err := SyncDir(path); err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(absolute)
	if err != nil {
		return nil, err
	}
	if err := root.Chmod(".", 0700); err != nil {
		_ = root.Close()
		return nil, err
	}
	return root, nil
}

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
	return errors.Join(SyncOpenDir(dir), dir.Close())
}

// SyncOpenDir keeps a caller's confined directory handle instead of resolving
// its path again. Windows has the same filesystem durability limitation as SyncDir.
func SyncOpenDir(dir *os.File) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	return dir.Sync()
}
