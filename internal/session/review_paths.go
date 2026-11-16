package session

import "path/filepath"

// ReviewPaths derives the v4 folder layout from a review identity path.
type ReviewPaths struct {
	Folder      string
	Review      string
	Snapshots   string
	Attachments string
}

// ReviewPathsFor returns the v4 folder-form paths for a review identity.
func ReviewPathsFor(identity string) ReviewPaths {
	return ReviewPaths{
		Folder:      identity,
		Review:      filepath.Join(identity, "review.json"),
		Snapshots:   filepath.Join(identity, "snapshots.json"),
		Attachments: filepath.Join(identity, "attachments"),
	}
}

// LSPScratch is where the sandbox lets the review's language servers write.
// Derived on demand: ReviewPathsFor runs on every save and load, which
// never need it.
func (p ReviewPaths) LSPScratch() string { return filepath.Join(p.Folder, "lsp-scratch") }

// reviewLockPath is the review write lock file for a review identity. It is
// not a ReviewPaths field: ReviewPathsFor runs on every review save and load,
// and only the lock code needs this path.
func reviewLockPath(identity string) string {
	return filepath.Join(identity, "review.lock")
}
