package lsp

import (
	"io/fs"
	"os"
	"path"
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

// goplsCacheDir is the GOPLSCACHE crit gives gopls: its default under the
// user cache dir is unwritable in the sandbox.
func goplsCacheDir(ws Workspace) string { return filepath.Join(ws.Scratch, "goplscache") }

// sweepGoplsCache deletes gopls cache files older than goplsCacheMaxAge.
// gopls does this itself, but its GC first runs 5 minutes after start and
// crit's 3-minute idle shutdown kills the server before that.
//
// The walk goes through an os.Root on the scratch: the server can write
// everything beneath it, a symlink in the cache's place included, and this
// runs in the daemon, unconfined. A Root refuses to follow one out.
func sweepGoplsCache(ws Workspace) {
	root, err := os.OpenRoot(ws.Scratch)
	if err != nil {
		return
	}
	defer root.Close()
	dir := path.Join("goplscache", "gopls")
	cutoff := time.Now().Add(-goplsCacheMaxAge)
	removed := 0
	_ = fs.WalkDir(root.FS(), dir, func(name string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil //nolint:nilerr // best effort
		}
		info, err := d.Info()
		if err != nil || !info.ModTime().Before(cutoff) {
			return nil //nolint:nilerr // best effort
		}
		if root.Remove(name) == nil {
			removed++
		}
		return nil
	})
	debugf(debugEnabled(), "", "gopls cache sweep removed %d stale files from %s", removed, filepath.Join(ws.Scratch, dir))
}
