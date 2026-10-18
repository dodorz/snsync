//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package lock

import (
	"errors"
	"os"
	"syscall"
)

var errWouldBlock = errors.New("lock is held by another process")

func lockFile(f *os.File, _ string) error {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == syscall.EWOULDBLOCK || err == syscall.EAGAIN {
		return errWouldBlock
	}
	return err
}

func unlockFile(f *os.File, _ string) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
