package lsp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The global node_modules is where the server on PATH is installed, found
// without npm: the npm on PATH may belong to another prefix, and its
// ~/.npmrc is unreadable in the sandbox.
func TestNpmGlobalRootsFollowTheServerBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	prefix, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	modules := filepath.Join(prefix, "lib", "node_modules")
	cli := filepath.Join(modules, "typescript-language-server", "lib", "cli.mjs")
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("#!/usr/bin/env node\n"), 0o755); err != nil { //nolint:gosec // LookPath needs it executable
		t.Fatal(err)
	}
	bin := filepath.Join(prefix, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, filepath.Join(bin, "typescript-language-server")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	noNpm := func(...string) ([]byte, error) { return nil, errors.New("npm must not be needed") }

	roots := npmGlobalRoots("", noNpm)
	if len(roots) != 1 || roots[0].Path != modules {
		t.Errorf("roots = %+v, want %s", roots, modules)
	}

	t.Setenv("PATH", t.TempDir())
	if roots := npmGlobalRoots("", noNpm); roots != nil {
		t.Errorf("roots without the server on PATH = %+v, want none", roots)
	}
}
