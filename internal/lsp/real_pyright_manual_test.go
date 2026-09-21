package lsp

// Opt-in integration tests against a real pyright (same env gate as the gopls
// one). Skipped unless enabled:
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealPyright -v
//
// Requires pyright-langserver on PATH (`npm install -g pyright`). The "venv"
// is just the directory layout crit looks for — pyright is never given an
// interpreter, which is the point.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pyLibSource = `def greet(name: str) -> str:
    """Return a friendly greeting for name."""
    return f"Hello, {name}!"
`

const pyAppSource = `import json

import mylib


def main() -> None:
    greeting = mylib.greet("world")
    print(json.dumps({"msg": greeting}))
`

// Position of "greet" in `    greeting = mylib.greet("world")`: 0-based line 6,
// the identifier spans characters 21-26.
const (
	pyGreetLine = 6
	pyGreetChar = 22
)

func requireRealPyright(t *testing.T) {
	t.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		t.Skip("set CRIT_LSP_REAL=1 to run against a real pyright")
	}
	if !LanguageForPath("x.py").Available() {
		t.Fatal("pyright-langserver not on PATH")
	}
}

// writePyProject lays out app.py and, when withVenv, a virtualenv-shaped
// .venv holding the third-party package mylib.
func writePyProject(t *testing.T, dir string, withVenv bool) string {
	t.Helper()
	if withVenv {
		lib := filepath.Join(dir, ".venv", "lib", "python3.13", "site-packages", "mylib")
		if err := os.MkdirAll(lib, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(lib, "__init__.py"), []byte(pyLibSource), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	app := filepath.Join(dir, "app.py")
	if err := os.WriteFile(app, []byte(pyAppSource), 0o644); err != nil {
		t.Fatal(err)
	}
	return app
}

func TestRealPyrightResolvesInTreeVenv(t *testing.T) {
	t.Parallel()
	requireRealPyright(t)

	dir := t.TempDir()
	app := writePyProject(t, dir, true)

	m := NewManager(dir, "", context.Background())
	defer m.Shutdown()

	start := time.Now()
	got, err := m.Hover(app, pyGreetLine, pyGreetChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	t.Logf("first hover took %s: %q", time.Since(start).Round(time.Millisecond), got)
	if strings.Contains(got, "Unknown") || !strings.Contains(got, "def greet(name: str) -> str") {
		t.Errorf("hover = %q, want greet's real signature (venv not picked up)", got)
	}

	locs, err := m.Definition(app, pyGreetLine, pyGreetChar)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	t.Logf("definition: %+v", locs)
	if len(locs) != 1 || !strings.Contains(filepath.ToSlash(locs[0].Path), ".venv/lib/python3.13/site-packages/mylib/__init__.py") {
		t.Errorf("definition = %+v, want mylib/__init__.py inside the venv", locs)
	}

	refs, err := m.References(app, pyGreetLine, pyGreetChar)
	if err != nil {
		t.Fatalf("References: %v", err)
	}
	t.Logf("references: %+v", refs)
	if len(refs) < 2 {
		t.Errorf("references = %+v, want the call site and the declaration", refs)
	}
}

// Control: no .venv, so nothing tells pyright where mylib is. This is the
// silent degradation the settings exist to prevent — it must look exactly like
// this, or the test above proves nothing.
func TestRealPyrightWithoutVenvCannotResolveThirdParty(t *testing.T) {
	t.Parallel()
	requireRealPyright(t)

	dir := t.TempDir()
	app := writePyProject(t, dir, false)

	m := NewManager(dir, "", context.Background())
	defer m.Shutdown()

	got, err := m.Hover(app, pyGreetLine, pyGreetChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	t.Logf("hover: %q", got)
	if !strings.Contains(got, "Unknown") {
		t.Errorf("hover = %q, want Unknown without a venv", got)
	}
}

// Range/PR focus: the server is rooted at a sparse worktree that has no .venv
// (it is untracked), while the working tree behind it does.
func TestRealPyrightResolvesVenvFromDepRoot(t *testing.T) {
	t.Parallel()
	requireRealPyright(t)

	worktree, depRoot := t.TempDir(), t.TempDir()
	app := writePyProject(t, worktree, false)
	writePyProject(t, depRoot, true)

	m := NewManager(worktree, depRoot, context.Background())
	defer m.Shutdown()

	got, err := m.Hover(app, pyGreetLine, pyGreetChar)
	if err != nil {
		t.Fatalf("Hover: %v", err)
	}
	t.Logf("hover: %q", got)
	if strings.Contains(got, "Unknown") || !strings.Contains(got, "def greet(name: str) -> str") {
		t.Errorf("hover = %q, want the venv borrowed from depRoot", got)
	}

	// The definition lands in depRoot's venv, outside the worktree: it must be
	// readable as a peek root or the UI cannot show it.
	locs, err := m.Definition(app, pyGreetLine, pyGreetChar)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) != 1 {
		t.Fatalf("definition = %+v, want 1 location", locs)
	}
	within := false
	for _, r := range m.PeekRoots() {
		t.Logf("peek root: %s (%s)", r.Path, r.Label)
		if strings.HasPrefix(locs[0].Path, r.Path) {
			within = true
		}
	}
	if !within {
		t.Errorf("definition %s is not under any peek root", locs[0].Path)
	}
}

// Stdlib definitions land in pyright's bundled typeshed and the real stdlib;
// both have to be peekable.
func TestRealPyrightStdlibDefinitionIsPeekable(t *testing.T) {
	t.Parallel()
	requireRealPyright(t)

	dir := t.TempDir()
	app := writePyProject(t, dir, true)

	m := NewManager(dir, "", context.Background())
	defer m.Shutdown()

	// `json.dumps` on line 8 (0-based 7): "    print(json.dumps(" — dumps at 16.
	locs, err := m.Definition(app, 7, 17)
	if err != nil {
		t.Fatalf("Definition: %v", err)
	}
	if len(locs) == 0 {
		t.Fatal("no definition for json.dumps")
	}
	roots := m.PeekRoots()
	for _, l := range locs {
		ok := false
		for _, r := range roots {
			if strings.HasPrefix(l.Path, r.Path) {
				ok = true
			}
		}
		t.Logf("definition %s peekable=%v", l.Path, ok)
		if !ok {
			t.Errorf("definition %s is not under any peek root %+v", l.Path, roots)
		}
	}
}
