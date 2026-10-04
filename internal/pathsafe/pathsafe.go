// Package pathsafe is the one symlink-resolving containment check for every
// place crit reads or serves files under a trusted root, so the
// security-sensitive validation cannot drift apart between call sites.
package pathsafe

import (
	"errors"
	"path/filepath"
	"strings"
)

// ErrNotFound reports that path itself does not resolve (missing file or
// dangling symlink); ErrDenied reports everything else — the root cannot be
// resolved, or the resolved path is not strictly inside the root. The
// distinction lets HTTP callers keep 404 vs 403 semantics.
var (
	ErrNotFound = errors.New("pathsafe: path does not resolve")
	ErrDenied   = errors.New("pathsafe: path escapes root")
)

// ResolveUnder returns path with symlinks resolved when it lies strictly
// inside root. The root itself is rejected — no caller legitimately reads or
// serves the root directory.
func ResolveUnder(path, root string) (string, error) {
	if root == "" {
		return "", ErrDenied
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", ErrNotFound
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", ErrDenied
	}
	if !strings.HasPrefix(resolvedPath, resolvedRoot+string(filepath.Separator)) {
		return "", ErrDenied
	}
	return resolvedPath, nil
}
