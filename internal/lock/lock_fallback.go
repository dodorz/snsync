//go:build !linux && !darwin && !freebsd && !openbsd && !netbsd && !dragonfly && !illumos && !windows

package lock

import (
	"errors"
	"os"
)

// errWouldBlock is returned by the platform lock implementation when another
// process holds the lock.
var errWouldBlock = errors.New("lock is held by another process")

// lockFile is a best-effort fallback for platforms without flock/LockFileEx.
// It is not released automatically when the process dies; the lock file must be
// removed manually in that case.
func lockFile(_ *os.File, path string) error {
	f, err := os.OpenFile(path+".owned", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if os.IsExist(err) {
			return errWouldBlock
		}
		return err
	}
	return f.Close()
}

func unlockFile(_ *os.File, path string) error {
	err := os.Remove(path + ".owned")
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
