package lsp

import (
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// goplsCacheMaxAge is the age gopls's own filecache GC evicts at: maxAge in
// gc() of golang.org/x/tools/gopls internal/filecache/filecache.go (v0.23.0),
// a constant gopls does not let users configure. Copied by hand, so it won't
// follow an upstream change. A mismatch is harmless: an entry evicted too
// early is rebuilt on next use, and keys are content hashes, so a stale one
// is never misread.
const goplsCacheMaxAge = 5 * 24 * time.Hour

// sweepGoplsCache deletes gopls cache files older than goplsCacheMaxAge.
// gopls does this itself, but its GC first runs 5 minutes after start and
// crit's 3-minute idle shutdown kills the server before that.
func sweepGoplsCache() {
	base := os.Getenv("GOPLSCACHE")
	if base == "" {
		var err error
		if base, err = os.UserCacheDir(); err != nil {
			return
		}
	}
	dir := filepath.Join(base, "gopls")
	if _, err := os.Stat(dir); err != nil {
		return
	}
	cutoff := time.Now().Add(-goplsCacheMaxAge)
	removed := 0
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best effort
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return nil //nolint:nilerr // best effort
		}
		if os.Remove(path) == nil {
			removed++
		}
		return nil
	})
	debugf(debugEnabled(), "", "gopls cache sweep removed %d stale files from %s", removed, dir)
}
