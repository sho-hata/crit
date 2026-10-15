//go:build !windows

package daemon

import (
	"os"
	"syscall"
)

// flockExclusive acquires an exclusive advisory lock on f, blocking until the
// lock is available. Released automatically when the process dies.
func FlockExclusive(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// flockExclusiveNB acquires an exclusive advisory lock on f without blocking.
// Returns an error immediately if another process holds the lock.
func flockExclusiveNB(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}

// unlockAndRemove deletes the lock file before releasing it, so a process
// waiting on the file cannot lock it just before it goes.
func unlockAndRemove(f *os.File) {
	os.Remove(f.Name())
	_ = Funlock(f)
	f.Close()
}

// Funlock releases an advisory lock previously acquired on f.
func Funlock(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
