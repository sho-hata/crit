//go:build windows

package daemon

import (
	"os"

	"github.com/sho-hata/crit/internal/flock"
)

// unlockAndRemove releases the lock file and then deletes it. Windows cannot
// delete an open file, so the delete fails harmlessly when another process
// has opened the file in the meantime.
func unlockAndRemove(f *os.File) {
	_ = flock.Unlock(f)
	f.Close()
	os.Remove(f.Name())
}
