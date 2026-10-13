package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
)

// jdtlsArgs gives jdt.ls a workspace (-data) of its own per root.
//
// The launcher's default is keyed by the root's base name only, so two
// reviews of directories that happen to share one — or every range-focus
// worktree, all named alike — would fight over the same Eclipse workspace,
// and the second server refuses to start because the first holds its lock.
// The dir is outside the repo, under ~/.crit, and is reused across sessions
// of the same root, so the server's index survives a restart.
func jdtlsArgs(root string) []string {
	return []string{"-data", jdtlsDataDir(root)}
}

// jdtlsDataDir is ~/.crit/lsp/jdtls/<hash of root>, created so the server
// finds its parent in place; the temp dir stands in when there is no home.
func jdtlsDataDir(root string) string {
	base, err := os.UserHomeDir()
	if err != nil {
		base = os.TempDir()
	}
	sum := sha256.Sum256([]byte(filepath.Clean(root)))
	dir := filepath.Join(base, ".crit", "lsp", "jdtls", hex.EncodeToString(sum[:])[:12])
	_ = os.MkdirAll(dir, 0o755) // jdt.ls reports its own error if this failed
	return dir
}
