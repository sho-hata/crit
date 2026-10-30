package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
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
		{"app/main.py", "python", "python"},
		{"stubs/types.pyi", "python", "python"},
		{"UPPER.PY", "python", "python"},
		{"infra/main.tf", "terraform", "terraform"},
		{"infra/prod.tfvars", "terraform", "terraform-vars"},
		{"src/Http/Controller.php", "php", "php"},
		{"templates/page.phtml", "php", "php"},
		// .hcl is shared with Packer, Nomad, Terragrunt, … and the Terraform
		// test files (.tftest.hcl) can't be told apart by extension alone.
		{"infra/run.tftest.hcl", "", ""},
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
	m.start = func(_ context.Context, _ string, lang *Language, _, _ map[string]any) (*Client, error) {
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

// writeTSLSInstall lays out a global npm install of typescript-language-server
// under prefix (bin/<name> -> lib/node_modules/<pkg>/lib/cli.mjs) and returns
// prefix/bin for PATH. tsserverNear reads this layout.
func writeTSLSInstall(t *testing.T, prefix string) string {
	t.Helper()
	return writeGlobalBin(t, prefix, "typescript-language-server", "typescript-language-server", filepath.Join("lib", "cli.mjs"))
}

// writeGlobalBin creates prefix/lib/node_modules/<pkg>/<entry> and the
// prefix/bin/<name> symlink npm makes for it; returns prefix/bin.
func writeGlobalBin(t *testing.T, prefix, name, pkg, entry string) string {
	t.Helper()
	target := filepath.Join(prefix, "lib", "node_modules", pkg, entry)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("#!/usr/bin/env node\n"), 0o755); err != nil { //nolint:gosec // executable stub
		t.Fatal(err)
	}
	bin := filepath.Join(prefix, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(bin, target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(bin, name)); err != nil {
		t.Fatal(err)
	}
	return bin
}

func TestTrustedTSServer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinked global bins are a Unix layout")
	}
	// Each case gets its own PATH, which is also the lookup cache key, so
	// the cases never see one another's result. npm is off PATH throughout,
	// so the global-root fallback cannot answer.

	t.Run("sibling of typescript-language-server", func(t *testing.T) {
		prefix := t.TempDir()
		bin := writeTSLSInstall(t, prefix)
		want := resolved(writeTSInstall(t, filepath.Join(prefix, "lib")))
		t.Setenv("PATH", bin)
		if got := trustedTSServer(); got != want {
			t.Errorf("trustedTSServer = %q, want %q", got, want)
		}
	})

	t.Run("nested under typescript-language-server", func(t *testing.T) {
		prefix := t.TempDir()
		bin := writeTSLSInstall(t, prefix)
		want := resolved(writeTSInstall(t, filepath.Join(prefix, "lib", "node_modules", "typescript-language-server")))
		t.Setenv("PATH", bin)
		if got := trustedTSServer(); got != want {
			t.Errorf("trustedTSServer = %q, want %q", got, want)
		}
	})

	// mise, volta and Homebrew keep each global package in its own prefix:
	// the server's prefix has no typescript, tsc's does.
	t.Run("behind tsc on PATH", func(t *testing.T) {
		tsls := writeTSLSInstall(t, t.TempDir())
		tsPrefix := t.TempDir()
		tsc := writeGlobalBin(t, tsPrefix, "tsc", "typescript", filepath.Join("bin", "tsc"))
		want := resolved(writeTSInstall(t, filepath.Join(tsPrefix, "lib")))
		t.Setenv("PATH", tsls+string(os.PathListSeparator)+tsc)
		if got := trustedTSServer(); got != want {
			t.Errorf("trustedTSServer = %q, want %q", got, want)
		}
	})

	t.Run("none", func(t *testing.T) {
		bin := writeTSLSInstall(t, t.TempDir())
		t.Setenv("PATH", bin)
		if got := trustedTSServer(); got != "" {
			t.Errorf("trustedTSServer = %q, want \"\"", got)
		}
		if err := tsRequires(""); err == nil {
			t.Error("tsRequires should fail without a trusted install")
		}
		// The binary is on PATH, yet the language is not available: starting
		// the server would let it pick the reviewed tree's typescript.
		lang := LanguageForPath("src/App.tsx")
		if lang.availableAt(time.Now()) {
			t.Error("typescript reported available without a trusted tsserver")
		}
	})
}

// A PATH that includes the reviewed tree's node_modules/.bin (direnv, a
// project shell) must not make the tree's own typescript the trusted one —
// that is the code the PR ships.
func TestTSInitOptionsRejectsReviewedTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinked bins are a Unix layout")
	}
	root := t.TempDir()
	repoTS := resolved(writeTSInstall(t, root))
	binDir := filepath.Join(root, "node_modules", ".bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tsc := filepath.Join(root, "node_modules", "typescript", "bin", "tsc")
	if err := os.MkdirAll(filepath.Dir(tsc), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tsc, []byte("#!/usr/bin/env node\n"), 0o755); err != nil { //nolint:gosec // executable stub
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "typescript", "bin", "tsc"), filepath.Join(binDir, "tsc")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	if got := trustedTSServer(); got != repoTS {
		t.Fatalf("trustedTSServer = %q, want the PATH install %q (the test's premise)", got, repoTS)
	}
	src := filepath.Join(root, "src", "App.tsx")
	if opts := tsInitOptions(root, src); opts != nil {
		t.Errorf("tsInitOptions inside the reviewed tree = %v, want nil", opts)
	}
	if err := tsRequires(root); err == nil {
		t.Error("tsRequires(root) should reject a tsserver inside root")
	}

	// The same install is fine for a review of some other tree.
	other := t.TempDir()
	opts := tsInitOptions(other, filepath.Join(other, "src", "App.tsx"))
	ts, ok := opts["tsserver"].(map[string]any)
	if !ok || ts["path"] != repoTS {
		t.Fatalf("tsInitOptions for another root = %v, want tsserver.path %q", opts, repoTS)
	}
	if opts["disableAutomaticTypingAcquisition"] != true {
		t.Errorf("disableAutomaticTypingAcquisition = %v, want true", opts["disableAutomaticTypingAcquisition"])
	}
	if err := tsRequires(other); err != nil {
		t.Errorf("tsRequires(other) = %v, want nil", err)
	}
}

// Registry wiring is a one-line omission away from the hooks silently not
// running.
func TestTypeScriptLanguageWiring(t *testing.T) {
	t.Parallel()

	lang := LanguageForPath("src/App.tsx")
	if lang == nil || lang.InitOptions == nil || lang.Requires == nil {
		t.Fatal("typescript language must define InitOptions and Requires")
	}
}

func TestGoLanguagePinsLocalToolchain(t *testing.T) {
	t.Parallel()

	lang := LanguageForPath("main.go")
	if lang == nil || !slices.Contains(lang.Env, "GOTOOLCHAIN=local") {
		t.Fatalf("go language Env = %v, want GOTOOLCHAIN=local", lang.Env)
	}
}

func TestAvailableCachesLookupPerPath(t *testing.T) {
	// Its own binary name, so no other test's lookups share the cache keys.
	lang := &Language{Name: "fake", Command: []string{"crit-fake-lsp-for-cache-test"}}
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	now := time.Now()

	if lang.availableAt(now) {
		t.Fatal("available before the binary exists")
	}
	name := lang.Command[0]
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if lang.availableAt(now.Add(availableTTL / 2)) {
		t.Error("within the TTL the earlier miss should be reused")
	}
	if !lang.availableAt(now.Add(availableTTL)) {
		t.Error("after the TTL the binary should be found")
	}

	// A different PATH is a different key: looked up even within the TTL.
	other := t.TempDir()
	t.Setenv("PATH", other)
	if lang.availableAt(now.Add(availableTTL)) {
		t.Error("PATH without the binary answered from the other PATH's entry")
	}
}

func TestTSOpenSessionFile(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		config string // relative to the parent of root; "" writes none
		file   string // relative to root
		want   bool
	}{
		{"no project anywhere", "", "src/a.ts", true},
		{"tsconfig.json at the root", "root/tsconfig.json", "src/a.ts", false},
		{"jsconfig.json in the file's package", "root/web/jsconfig.json", "web/a.js", false},
		{"a sibling package's tsconfig.json", "root/api/tsconfig.json", "web/a.ts", true},
		{"tsconfig.json above the root", "tsconfig.json", "src/a.ts", true},
		{"JavaScript under a tsconfig.json, which may lack allowJs", "root/tsconfig.json", "src/a.js", true},
		{"JavaScript under a jsconfig.json", "root/jsconfig.json", "src/a.mjs", false},
		{"TypeScript under a jsconfig.json", "root/jsconfig.json", "src/a.tsx", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			parent := t.TempDir()
			root := filepath.Join(parent, "root")
			for _, p := range []string{tc.config, filepath.Join("root", tc.file)} {
				if p == "" {
					continue
				}
				abs := filepath.Join(parent, p)
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(abs, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if got := tsOpenSessionFile(root, filepath.Join(root, tc.file)); got != tc.want {
				t.Errorf("tsOpenSessionFile = %v, want %v", got, tc.want)
			}
		})
	}
}
