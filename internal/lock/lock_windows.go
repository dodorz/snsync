//go:build windows

package lock

import (
	"errors"
	"os"
	"syscall"
	"unsafe"
)

var errWouldBlock = errors.New("lock is held by another process")

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileFailImmediately = 0x00000001
	lockfileExclusiveLock   = 0x00000002
	errorLockViolation      = syscall.Errno(33) // ERROR_LOCK_VIOLATION
)

func lockFile(f *os.File, _ string) error {
	var ol syscall.Overlapped
	r1, _, e1 := procLockFileEx.Call(
		uintptr(f.Fd()),
		lockfileExclusiveLock|lockfileFailImmediately,
		0, // reserved
		1, // lock one byte
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 != 0 {
		return nil
	}
	if e1 == errorLockViolation {
		return errWouldBlock
	}
	return e1
}

func unlockFile(f *os.File, _ string) error {
	var ol syscall.Overlapped
	r1, _, e1 := procUnlockFileEx.Call(
		uintptr(f.Fd()),
		0, // reserved
		1,
		0,
		uintptr(unsafe.Pointer(&ol)),
	)
	if r1 == 0 {
		return e1
	}
	return nil
}
