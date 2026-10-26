//go:build windows

package flock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// Windows locks byte ranges, not files; locking the first byte is the usual
// stand-in for a whole-file flock.
const (
	lockLenLow  uint32 = 1
	lockLenHigh uint32 = 0
)

// Lock blocks until it holds an exclusive lock on f.
func Lock(f *os.File) error {
	return lockFileEx(f, windows.LOCKFILE_EXCLUSIVE_LOCK)
}

// TryLock takes an exclusive lock on f without blocking. It returns false and
// a nil error when another holder has the lock.
func TryLock(f *os.File) (bool, error) {
	err := lockFileEx(f, windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return false, nil
	}
	return err == nil, err
}

// Unlock releases a lock taken by Lock or TryLock.
func Unlock(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, lockLenLow, lockLenHigh, new(windows.Overlapped))
}

func lockFileEx(f *os.File, flags uint32) error {
	return windows.LockFileEx(windows.Handle(f.Fd()), flags, 0, lockLenLow, lockLenHigh, new(windows.Overlapped))
}
