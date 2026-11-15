package lsp

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// installTSServer lays out an npm prefix: <prefix>/lib/node_modules holds the
// package and <prefix>/bin links to its entry point. It returns both dirs.
func installTSServer(t *testing.T, prefix string) (bin, modules string) {
	t.Helper()
	modules = filepath.Join(prefix, "lib", "node_modules")
	cli := filepath.Join(modules, "typescript-language-server", "lib", "cli.mjs")
	if err := os.MkdirAll(filepath.Dir(cli), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cli, []byte("#!/usr/bin/env node\n"), 0o755); err != nil { //nolint:gosec // LookPath needs it executable
		t.Fatal(err)
	}
	bin = filepath.Join(prefix, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(cli, filepath.Join(bin, "typescript-language-server")); err != nil {
		t.Fatal(err)
	}
	return bin, modules
}

func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func noNpm(...string) ([]byte, error) { return nil, errors.New("npm must not be needed") }

// The global node_modules is where the server on PATH is installed, found
// without npm: the npm on PATH may belong to another prefix, and its
// ~/.npmrc is unreadable in the sandbox.
func TestNpmGlobalRootsFollowTheServerBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	bin, modules := installTSServer(t, realTempDir(t))
	t.Setenv("PATH", bin)

	roots := npmGlobalRoots("", noNpm)
	if len(roots) != 1 || roots[0].Path != modules {
		t.Errorf("roots = %+v, want %s", roots, modules)
	}

	t.Setenv("PATH", t.TempDir())
	if roots := npmGlobalRoots("", noNpm); roots != nil {
		t.Errorf("roots without the server on PATH = %+v, want none", roots)
	}
}

// The sandbox never starts a server from the tree under review, so a copy
// in the repository's node_modules/.bin, ahead on PATH, is not the one that
// runs and its node_modules is not the global one.
func TestNpmGlobalRootsSkipAServerInTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	repo := realTempDir(t)
	localBin, _ := installTSServer(t, filepath.Join(repo, "node_modules", ".prefix"))
	globalBin, modules := installTSServer(t, realTempDir(t))
	t.Setenv("PATH", "relative/bin"+string(os.PathListSeparator)+localBin+string(os.PathListSeparator)+globalBin)

	roots := npmGlobalRoots(repo, noNpm)
	if len(roots) != 1 || roots[0].Path != modules {
		t.Errorf("roots = %+v, want the global %s", roots, modules)
	}
}

// A version manager's shim (asdf) is a script, not a link into the package,
// so the path says nothing about where the package lives: npm is asked.
func TestNpmGlobalRootsAskNpmBehindAShim(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script")
	}
	shims := realTempDir(t)
	shim := filepath.Join(shims, "typescript-language-server")
	if err := os.WriteFile(shim, []byte("#!/bin/sh\nexec asdf exec typescript-language-server \"$@\"\n"), 0o755); err != nil { //nolint:gosec // LookPath needs it executable
		t.Fatal(err)
	}
	t.Setenv("PATH", shims)
	modules := filepath.Join(realTempDir(t), "lib", "node_modules")
	var asked [][]string
	npm := func(argv ...string) ([]byte, error) {
		asked = append(asked, argv)
		return []byte(modules + "\n"), nil
	}

	roots := npmGlobalRoots("", npm)
	if len(roots) != 1 || roots[0].Path != modules {
		t.Errorf("roots = %+v, want npm's answer %s (asked %v)", roots, modules, asked)
	}
}
