package lsp

import "path/filepath"

// jdtlsArgs gives jdt.ls an Eclipse workspace (-data) in the scratch. The
// launcher's default is keyed by the root's base name, so reviews of
// same-named roots (every range-focus worktree) would contend for one lock.
func jdtlsArgs(ws Workspace) []string {
	return []string{
		"-data", filepath.Join(ws.Scratch, "data"),
		"-configuration", filepath.Join(ws.Scratch, "configuration"),
	}
}
