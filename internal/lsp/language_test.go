package lsp

import (
	"context"
	"encoding/json"
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
	m := NewManager(t.TempDir(), context.Background())
	m.start = func(_ context.Context, _ string, lang *Language) (*Client, error) {
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
