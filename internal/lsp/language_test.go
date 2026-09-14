package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLanguageForPath(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path string
		lang string // "" = no language
		id   string
	}{
		{"internal/server/lsp_handlers.go", "go", "go"},
		{"web/src/App.tsx", "typescript", "typescriptreact"},
		{"web/src/util.ts", "typescript", "typescript"},
		{"web/src/legacy.js", "typescript", "javascript"},
		{"web/src/component.jsx", "typescript", "javascriptreact"},
		{"web/src/mod.mts", "typescript", "typescript"},
		{"UPPER.TS", "typescript", "typescript"},
		{"README.md", "", ""},
		{"Makefile", "", ""},
		{"noext", "", ""},
	}
	for _, tc := range cases {
		lang := LanguageForPath(tc.path)
		if tc.lang == "" {
			if lang != nil {
				t.Errorf("LanguageForPath(%q) = %s, want nil", tc.path, lang.Name)
			}
			continue
		}
		if lang == nil {
			t.Errorf("LanguageForPath(%q) = nil, want %s", tc.path, tc.lang)
			continue
		}
		if lang.Name != tc.lang {
			t.Errorf("LanguageForPath(%q) = %s, want %s", tc.path, lang.Name, tc.lang)
		}
		if got := lang.LanguageID(tc.path); got != tc.id {
			t.Errorf("LanguageID(%q) = %q, want %q", tc.path, got, tc.id)
		}
	}
}

func TestManagerSpawnsOneServerPerLanguage(t *testing.T) {
	t.Parallel()

	var langs []string
	h := &managerHarness{handler: hoverOK}
	m := NewManager(t.TempDir(), "", context.Background())
	m.start = func(_ context.Context, _ string, lang *Language, _ map[string]any) (*Client, error) {
		langs = append(langs, lang.Name)
		fs := startFake(h.handler)
		h.mu.Lock()
		h.servers = append(h.servers, fs)
		h.mu.Unlock()
		return fs.client, nil
	}
	t.Cleanup(m.Shutdown)

	goFile := writeGoFile(t, m.root, "main.go", "package main\n")
	tsFile := writeGoFile(t, m.root, "app.ts", "const x = 1\n")

	if _, err := m.Hover(goFile, 0, 0); err != nil {
		t.Fatalf("Hover(go): %v", err)
	}
	if _, err := m.Hover(tsFile, 0, 0); err != nil {
		t.Fatalf("Hover(ts): %v", err)
	}
	// Same languages again must reuse the running servers.
	if _, err := m.Hover(goFile, 0, 0); err != nil {
		t.Fatalf("Hover(go, 2): %v", err)
	}
	if _, err := m.Hover(tsFile, 0, 0); err != nil {
		t.Fatalf("Hover(ts, 2): %v", err)
	}

	if len(langs) != 2 || langs[0] != "go" || langs[1] != "typescript" {
		t.Fatalf("spawned languages = %v, want [go typescript]", langs)
	}

	// The TypeScript server must receive the TypeScript languageId, not Go's.
	var tsOpenID string
	fs := h.server(1)
	fs.mu.Lock()
	for _, n := range fs.notifications {
		if n.Method != "textDocument/didOpen" {
			continue
		}
		var p struct {
			TextDocument struct {
				LanguageID string `json:"languageId"`
			} `json:"textDocument"`
		}
		if err := json.Unmarshal(n.Params, &p); err != nil {
			t.Errorf("bad didOpen params: %v", err)
		}
		tsOpenID = p.TextDocument.LanguageID
	}
	fs.mu.Unlock()
	if tsOpenID != "typescript" {
		t.Errorf("didOpen languageId = %q, want %q", tsOpenID, "typescript")
	}
}

func TestManagerRejectsUnsupportedExtension(t *testing.T) {
	t.Parallel()

	m, h := newManagerHarness(t, hoverOK)
	file := writeGoFile(t, m.root, "notes.md", "# notes\n")
	if _, err := m.Hover(file, 0, 0); err == nil {
		t.Error("Hover on unsupported extension should error")
	}
	if h.spawns.Load() != 0 {
		t.Errorf("spawns = %d, want 0 (no server for unsupported files)", h.spawns.Load())
	}
}

// writeTSInstall creates a node_modules/typescript/lib/tsserver.js under dir
// and returns the tsserver.js path, standing in for an installed dependency.
func writeTSInstall(t *testing.T, dir string) string {
	t.Helper()
	lib := filepath.Join(dir, "node_modules", "typescript", "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", lib, err)
	}
	tsserver := filepath.Join(lib, "tsserver.js")
	if err := os.WriteFile(tsserver, []byte("// stub\n"), 0o600); err != nil {
		t.Fatalf("write %s: %v", tsserver, err)
	}
	return tsserver
}

func TestFindTSServer(t *testing.T) {
	t.Parallel()

	// Monorepo: the dependency belongs to the package, not the repo root —
	// the layout that made typescript-language-server exit at initialize.
	t.Run("nearest install wins", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		rootTS := writeTSInstall(t, root)
		pkg := filepath.Join(root, "frontend")
		pkgTS := writeTSInstall(t, pkg)
		src := filepath.Join(pkg, "src", "App.tsx")
		if got := findTSServer(root, src); got != pkgTS {
			t.Errorf("findTSServer = %q, want the package install %q", got, pkgTS)
		}
		// A file outside the package still falls back to the root install.
		if got := findTSServer(root, filepath.Join(root, "tools", "gen.ts")); got != rootTS {
			t.Errorf("findTSServer (root file) = %q, want %q", got, rootTS)
		}
	})

	t.Run("install at root only", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		rootTS := writeTSInstall(t, root)
		src := filepath.Join(root, "src", "deep", "util.ts")
		if got := findTSServer(root, src); got != rootTS {
			t.Errorf("findTSServer = %q, want %q", got, rootTS)
		}
	})

	t.Run("no install", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		if got := findTSServer(root, filepath.Join(root, "src", "util.ts")); got != "" {
			t.Errorf("findTSServer = %q, want \"\" (server resolves on its own)", got)
		}
	})

	// The walk must stop at root: a path outside the workspace is not ours to
	// resolve dependencies for, and walking on would escape into the parent
	// tree (and, on a shallow root, towards /).
	t.Run("path outside root", func(t *testing.T) {
		t.Parallel()
		base := t.TempDir()
		root := filepath.Join(base, "repo")
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		outside := filepath.Join(base, "other")
		writeTSInstall(t, outside)
		if got := findTSServer(root, filepath.Join(outside, "src", "util.ts")); got != "" {
			t.Errorf("findTSServer = %q, want \"\" for a file outside root", got)
		}
	})
}

func TestTSInitOptions(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	src := filepath.Join(root, "frontend", "src", "App.tsx")

	if opts := tsInitOptions(root, src); opts != nil {
		t.Errorf("tsInitOptions without a local install = %v, want nil", opts)
	}

	tsserver := writeTSInstall(t, filepath.Join(root, "frontend"))
	opts := tsInitOptions(root, src)
	ts, ok := opts["tsserver"].(map[string]any)
	if !ok {
		t.Fatalf("tsInitOptions = %v, want a tsserver entry", opts)
	}
	if ts["path"] != tsserver {
		t.Errorf("tsserver.path = %v, want %q", ts["path"], tsserver)
	}
}

// The typescript language must actually carry the hook — registry wiring is
// what the fix turns on, and it is a one-line omission away from silence.
func TestTypeScriptLanguageSendsInitOptions(t *testing.T) {
	t.Parallel()

	lang := LanguageForPath("src/App.tsx")
	if lang == nil || lang.InitOptions == nil {
		t.Fatal("typescript language must define InitOptions")
	}
	root := t.TempDir()
	writeTSInstall(t, root)
	if lang.InitOptions(root, filepath.Join(root, "src", "App.tsx")) == nil {
		t.Error("InitOptions returned nil with an install present")
	}
}
