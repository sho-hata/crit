//go:build !windows

package flock

import (
	"errors"
	"os"
	"syscall"
)

// Lock blocks until it holds an exclusive lock on f.
func Lock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// TryLock takes an exclusive lock on f without blocking. It returns false and
// a nil error when another holder has the lock.
func TryLock(f *os.File) (bool, error) {
	err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) {
		return false, nil
	}
	return err == nil, err
}

// Unlock releases a lock taken by Lock or TryLock.
func Unlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
