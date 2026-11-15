package lsp

// Opt-in integration tests against a real intelephense (same CRIT_LSP_REAL
// gate as the other real-server tests). Skipped unless enabled:
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealPHP -v
//
// Requires intelephense on PATH (npm install -g intelephense). No PHP needed:
// intelephense is a static analyzer and never runs an interpreter.

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const phpGreeterSource = `<?php

namespace App;

/**
 * A friendly greeter.
 */
class Greeter
{
    /**
     * Returns a friendly greeting for name.
     */
    public function greet(string $name): string
    {
        return "Hello, {$name}";
    }
}
`

const phpAppSource = `<?php

use App\Greeter;

echo (new Greeter())->greet('world');
`

const phpVendorSource = `<?php

namespace Acme;

/**
 * A widget from a Composer package.
 */
class Widget
{
    public function spin(): int
    {
        return 1;
    }
}
`

const phpVendorUseSource = `<?php

use Acme\Widget;

echo (new Widget())->spin();
echo strlen('x');
`

// Positions (0-based line, UTF-16 character).
const (
	phpGreetUseLine, phpGreetUseChar = 4, 22 // greet in phpAppSource
	// intelephense answers a definition with the declaration's whole range,
	// which starts at its docblock, not at "public function greet".
	phpGreetDefLine                = 9
	phpSpinUseLine, phpSpinUseChar = 4, 21 // spin in phpVendorUseSource
	phpStrlenLine, phpStrlenChar   = 5, 6  // strlen in phpVendorUseSource
)

func requireRealIntelephense(t *testing.T) {
	t.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real intelephense")
	}
	if !LanguageForPath("x.php").Available() {
		t.Fatal("intelephense not on PATH")
	}
}

func writeTree(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, src := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func listTree(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(files)
	return files
}

// writePHPProject lays out app.php, plus a Composer vendor dir holding
// Acme\Widget when withVendor, and returns app.php's path.
func writePHPProject(t *testing.T, dir string, withVendor bool) string {
	t.Helper()
	files := map[string]string{"app.php": phpVendorUseSource}
	if withVendor {
		files["vendor/composer/installed.json"] = "{}\n"
		files["vendor/acme/widget/src/Widget.php"] = phpVendorSource
	}
	writeTree(t, dir, files)
	return filepath.Join(dir, "app.php")
}

func underPeekRoot(path string, roots []PeekRoot) bool {
	for _, r := range roots {
		if strings.HasPrefix(path, r.Path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Range/PR focus: the server is rooted at a sparse worktree that has no
// vendor/ (it is untracked), while the working tree behind it does.
func TestRealPHPResolvesVendorFromDepRoot(t *testing.T) {
	t.Parallel()
	requireRealIntelephense(t)

	worktree, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	depRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app := writePHPProject(t, worktree, false)
	writePHPProject(t, depRoot, true)

	m := NewManager(worktree, depRoot, t.TempDir(), context.Background())
	defer m.Shutdown()

	got, err := m.Hover(app, phpSpinUseLine, phpSpinUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if !strings.Contains(got, `Acme\\Widget::spin`) {
		t.Errorf("hover = %q, want the vendor borrowed from depRoot", got)
	}

	// The definition lands in depRoot's vendor, outside the worktree: it must
	// be readable as a peek root or the UI cannot show it.
	locs, err := m.Definition(app, phpSpinUseLine, phpSpinUseChar)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	want := filepath.Join(depRoot, "vendor", "acme", "widget", "src", "Widget.php")
	if len(locs) != 1 || locs[0].Path != want {
		t.Fatalf("definition = %+v, want %s", locs, want)
	}
	if roots := m.PeekRoots(); !underPeekRoot(locs[0].Path, roots) {
		t.Errorf("definition %s is not under any peek root %+v", locs[0].Path, roots)
	}
}

// The control for the test above: without a vendor dir anywhere the symbol
// does not resolve, so a pass there is the include path's doing.
func TestRealPHPWithoutVendorCannotResolveThirdParty(t *testing.T) {
	t.Parallel()
	requireRealIntelephense(t)

	dir := t.TempDir()
	app := writePHPProject(t, dir, false)

	m := NewManager(dir, "", t.TempDir(), context.Background())
	defer m.Shutdown()

	got, err := m.Hover(app, phpSpinUseLine, phpSpinUseChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if got != "" {
		t.Errorf("hover = %q, want nothing without a vendor dir", got)
	}
}

// Definitions of built-ins land in the stubs bundled with intelephense.
func TestRealPHPBuiltinDefinitionIsPeekable(t *testing.T) {
	t.Parallel()
	requireRealIntelephense(t)

	dir := t.TempDir()
	app := writePHPProject(t, dir, false)

	m := NewManager(dir, "", t.TempDir(), context.Background())
	defer m.Shutdown()

	locs, err := m.Definition(app, phpStrlenLine, phpStrlenChar)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) == 0 {
		t.Fatal("no definition for strlen")
	}
	roots := m.PeekRoots()
	for _, l := range locs {
		if !underPeekRoot(l.Path, roots) {
			t.Errorf("definition %s is not under any peek root %+v", l.Path, roots)
		}
	}
}

// Indexing the repo must not write into it: the cache goes to intelephense's
// own storage outside the workspace.
func TestRealPHPWritesNothingIntoTheRepo(t *testing.T) {
	t.Parallel()
	requireRealIntelephense(t)

	dir := t.TempDir()
	app := writePHPProject(t, dir, true)
	before := listTree(t, dir)

	m := NewManager(dir, "", t.TempDir(), context.Background())
	t.Cleanup(m.Shutdown) // a failed Hover must not leak the server
	if _, err := m.Hover(app, phpSpinUseLine, phpSpinUseChar); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	// Stop it before listing: a still-running server could write afterwards.
	m.Shutdown()

	if after := listTree(t, dir); !slices.Equal(after, before) {
		t.Errorf("repo files changed:\nbefore %v\nafter  %v", before, after)
	}
}
