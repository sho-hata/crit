//go:build !windows

package daemon

import (
	"os"

	"github.com/sho-hata/crit/internal/flock"
)

// unlockAndRemove deletes the lock file before releasing it, so a process
// waiting on the file cannot lock it just before it goes.
func unlockAndRemove(f *os.File) {
	os.Remove(f.Name())
	_ = flock.Unlock(f)
	f.Close()
}
