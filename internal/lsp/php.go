package lsp

import (
	"os/exec"
	"path/filepath"
	"strings"
)

// phpConfigSettings points intelephense at the nearest Composer vendor dir.
//
// intelephense indexes the workspace only, so it already finds a vendor/ under
// the repo root; this matters under range/PR focus, where the sparse worktree
// has no vendor/ (it is untracked) and Manager.depRoot borrows the working
// tree's. It reads includePaths from a workspace/configuration pull. Giving it
// a dir that is also in the workspace is harmless: files are indexed once per
// URI. intelephense is a static analyzer and runs nothing from these dirs.
func phpConfigSettings(root, absPath string) map[string]any {
	vendor := findComposerVendor(root, absPath)
	if vendor == "" {
		return nil
	}
	return map[string]any{
		"intelephense": map[string]any{
			"environment": map[string]any{"includePaths": []string{vendor}},
		},
	}
}

// findComposerVendor walks up from absPath's directory through root
// (inclusive), like findSitePackages. The nearest wins because a monorepo
// keeps one per package.
func findComposerVendor(root, absPath string) string {
	root = filepath.Clean(root)
	dir := filepath.Dir(absPath)
	if rel, err := filepath.Rel(root, dir); err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	for {
		if v := composerVendor(dir); v != "" {
			return v
		}
		if dir == root {
			return ""
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// composerVendor requires vendor/composer, which Composer always writes, so a
// Go or Ruby vendor/ is not mistaken for one.
func composerVendor(dir string) string {
	vendor := filepath.Join(dir, "vendor")
	if isDir(filepath.Join(vendor, "composer")) {
		return vendor
	}
	return ""
}

// phpExtraRoots:
//   - the Composer vendor dir in root (under range/PR focus the working tree,
//     outside the sparse worktree the server runs in)
//   - intelephense's bundled stubs, where definitions of built-in functions
//     and classes (strlen, ArrayObject) land
//
// Each source is optional; only a lookup that finds nothing at all is nil.
func phpExtraRoots(root string) []PeekRoot {
	var roots []PeekRoot
	if v := composerVendor(root); v != "" {
		roots = append(roots, PeekRoot{Path: v, Label: "$VENDOR", LocalEnv: true})
	}
	if dir := intelephenseStubs(); dir != "" {
		roots = append(roots, PeekRoot{Path: dir, Label: "$PHP_STUBS"})
	}
	if len(roots) == 0 {
		return nil
	}
	return roots
}

// intelephenseStubs returns "" when the binary is not laid out like the npm
// package; built-in definitions then still answer, but their peek is
// unreadable.
func intelephenseStubs() string {
	bin, err := exec.LookPath("intelephense")
	if err != nil {
		return ""
	}
	// npm links the bin to lib/intelephense.js; the stubs sit next to it.
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return ""
	}
	dir := filepath.Join(filepath.Dir(real), "stub")
	if isDir(dir) {
		return dir
	}
	return ""
}
