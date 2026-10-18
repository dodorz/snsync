// Package lock provides a cross-process advisory lock so that two snsync
// processes never reconcile the same state directory at the same time.
//
// The lock is backed by the operating system (flock on Unix, LockFileEx on
// Windows), so it is released automatically when the owning process exits or
// is killed. The lock file itself is only a rendezvous point and may be left on
// disk after a run.
package lock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// ErrHeld is returned by Acquire when another process already holds the lock.
var ErrHeld = errors.New("another snsync is already running")

// Lock is an acquired advisory lock.
type Lock struct {
	f    *os.File
	path string
}

// Acquire takes an exclusive, non-blocking lock on path, creating it (and its
// parent directory) when needed. It returns ErrHeld when another process owns
// the lock.
func Acquire(path string) (*Lock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock file %s: %w", path, err)
	}
	if err := lockFile(f, path); err != nil {
		_ = f.Close()
		if errors.Is(err, errWouldBlock) {
			return nil, ErrHeld
		}
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}

	// Best effort: record the owner's PID so the file is useful to humans.
	// Correctness never depends on its contents; the OS enforces the lock.
	_ = f.Truncate(0)
	_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)

	return &Lock{f: f, path: path}, nil
}

// Release drops the lock. It is safe to call more than once.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	err := unlockFile(l.f, l.path)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}
