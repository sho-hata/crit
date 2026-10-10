package lsp

// Cross-language contract and latency benchmarks against the real servers
// (same CRIT_LSP_REAL gate as the other real-server tests):
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealContract -v
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run '^$' -bench BenchmarkRealLSP
//
// Every registered language must have a realCases fixture
// (TestRealCasesCoverRegistry runs ungated), so a new language cannot ship
// without both. CI compares the benchmarks against the base branch; see
// scripts/bench-lsp.sh.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type realPos struct{ line, char int }

// realCase is one language's fixture: a symbol declared in one file and used
// in another, so hover, definition and references all cross a file boundary.
type realCase struct {
	files     map[string]string // repo-relative path → content
	file      string            // the file every request is sent to
	use       realPos           // a use of the symbol
	hoverWant string            // substring of the hover at use
	defFile   string            // where Definition(use) lands
	defLine   int
	refsAt    realPos // terraform-ls answers references only from the declaration
	refsMin   int
	blank     realPos // nothing here: must answer empty, not error
}

var realCases = map[string]realCase{
	"go": {
		files: map[string]string{
			"go.mod": "module contract\n\ngo 1.22\n",
			"greet.go": "package main\n\n" +
				"// Greet returns a friendly greeting for name.\n" +
				"func Greet(name string) string {\n\treturn \"Hello, \" + name\n}\n",
			"main.go": "package main\n\nimport \"fmt\"\n\n" +
				"func main() {\n\tfmt.Println(Greet(\"world\"))\n}\n",
		},
		file:      "main.go",
		use:       realPos{5, 14},
		hoverWant: "friendly greeting",
		defFile:   "greet.go",
		defLine:   3,
		refsAt:    realPos{5, 14},
		refsMin:   2,
		blank:     realPos{1, 0},
	},
	"typescript": {
		files: map[string]string{
			"tsconfig.json": `{"compilerOptions": {"strict": true}}` + "\n",
			"greet.ts": "/** Returns a friendly greeting for name. */\n" +
				"export function greet(name: string): string {\n  return `Hello, ${name}`;\n}\n",
			"app.ts": "import { greet } from \"./greet\";\n\nconsole.log(greet(\"world\"));\n",
		},
		file:      "app.ts",
		use:       realPos{2, 13},
		hoverWant: "friendly greeting",
		defFile:   "greet.ts",
		defLine:   1,
		refsAt:    realPos{2, 13},
		refsMin:   2,
		blank:     realPos{1, 0},
	},
	"python": {
		files: map[string]string{
			"mylib.py": "def greet(name: str) -> str:\n" +
				"    \"\"\"Return a friendly greeting for name.\"\"\"\n    return f\"Hello, {name}!\"\n",
			"app.py": "from mylib import greet\n\nprint(greet(\"world\"))\n",
		},
		file:      "app.py",
		use:       realPos{2, 7},
		hoverWant: "friendly greeting",
		defFile:   "mylib.py",
		defLine:   0,
		refsAt:    realPos{2, 7},
		refsMin:   2,
		blank:     realPos{1, 0},
	},
	"terraform": {
		files: map[string]string{
			"versions.tf":         tfVersionsSource,
			"main.tf":             tfMainSource,
			"modules/net/main.tf": tfNetSource,
		},
		file:      "main.tf",
		use:       realPos{tfVpcIDLine, tfVpcIDChar},
		hoverWant: "The VPC id",
		defFile:   "modules/net/main.tf",
		defLine:   0,
		refsAt:    realPos{tfRegionDeclLine, tfRegionDecl},
		refsMin:   1,
		blank:     realPos{tfBlankLine, tfBlankChar},
	},
}

// TestRealCasesCoverRegistry is ungated: it fails in the ordinary unit run
// as soon as a language is registered without a contract fixture.
func TestRealCasesCoverRegistry(t *testing.T) {
	t.Parallel()
	for _, lang := range languages {
		c, ok := realCases[lang.Name]
		if !ok {
			t.Errorf("language %q has no realCases fixture", lang.Name)
			continue
		}
		if got := LanguageForPath(c.file); got != lang {
			t.Errorf("realCases[%q].file = %q, which is not a %s file", lang.Name, c.file, lang.Name)
		}
	}
	for name := range realCases {
		found := false
		for _, lang := range languages {
			found = found || lang.Name == name
		}
		if !found {
			t.Errorf("realCases has %q, which is not a registered language", name)
		}
	}
}

// writeRealCase lays out c under a fresh temp dir and returns the dir with
// symlinks resolved (macOS temp dirs live behind /var → /private/var, and
// servers report the resolved form).
func writeRealCase(tb testing.TB, c realCase) string {
	tb.Helper()
	dir, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	for name, src := range c.files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			tb.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

func requireRealLanguage(tb testing.TB, lang *Language) {
	tb.Helper()
	if os.Getenv("CRIT_LSP_REAL") == "" {
		tb.Skip("set CRIT_LSP_REAL=1 to run against the real language servers")
	}
	if !lang.Available() {
		tb.Fatalf("%s not on PATH", lang.Command[0])
	}
}

func TestRealContract(t *testing.T) {
	t.Parallel()
	for _, lang := range languages {
		c, ok := realCases[lang.Name]
		if !ok {
			continue // reported by TestRealCasesCoverRegistry
		}
		t.Run(lang.Name, func(t *testing.T) {
			t.Parallel()
			requireRealLanguage(t, lang)
			dir := writeRealCase(t, c)
			file := filepath.Join(dir, c.file)

			m := NewManager(dir, "", context.Background())
			defer m.Shutdown()

			hover, err := m.Hover(file, c.use.line, c.use.char)
			if err != nil {
				t.Fatalf("Hover: %v", err)
			}
			if !strings.Contains(hover, c.hoverWant) {
				t.Errorf("hover = %q, want it to contain %q", hover, c.hoverWant)
			}

			locs, err := m.Definition(file, c.use.line, c.use.char)
			if err != nil {
				t.Fatalf("Definition: %v", err)
			}
			wantDef := filepath.Join(dir, filepath.FromSlash(c.defFile))
			found := false
			for _, l := range locs {
				found = found || (l.Path == wantDef && l.Line == c.defLine)
			}
			if !found {
				t.Errorf("definition = %+v, want %s line %d among them", locs, c.defFile, c.defLine)
			}

			refs, err := m.References(file, c.refsAt.line, c.refsAt.char)
			if err != nil {
				t.Fatalf("References: %v", err)
			}
			if len(refs) < c.refsMin {
				t.Errorf("references = %+v, want at least %d", refs, c.refsMin)
			}

			hover, err = m.Hover(file, c.blank.line, c.blank.char)
			if err != nil || hover != "" {
				t.Errorf("Hover(blank) = %q, %v; want empty and no error", hover, err)
			}
		})
	}
}

// BenchmarkRealLSP measures what a reviewer waits for: cold is spawn +
// handshake + the first hover (the lazy-start path), the rest are requests
// against a warm server.
func BenchmarkRealLSP(b *testing.B) {
	for _, lang := range languages {
		c, ok := realCases[lang.Name]
		if !ok {
			continue
		}
		b.Run(lang.Name, func(b *testing.B) {
			requireRealLanguage(b, lang)
			dir := writeRealCase(b, c)
			file := filepath.Join(dir, c.file)

			b.Run("cold", func(b *testing.B) {
				for b.Loop() {
					m := NewManager(dir, "", context.Background())
					if _, err := m.Hover(file, c.use.line, c.use.char); err != nil {
						b.Fatalf("Hover: %v", err)
					}
					b.StopTimer()
					m.Shutdown()
					b.StartTimer()
				}
			})

			m := NewManager(dir, "", context.Background())
			defer m.Shutdown()
			if _, err := m.Hover(file, c.use.line, c.use.char); err != nil {
				b.Fatalf("warm-up Hover: %v", err)
			}
			b.Run("hover", func(b *testing.B) {
				for b.Loop() {
					if _, err := m.Hover(file, c.use.line, c.use.char); err != nil {
						b.Fatalf("Hover: %v", err)
					}
				}
			})
			b.Run("definition", func(b *testing.B) {
				for b.Loop() {
					if _, err := m.Definition(file, c.use.line, c.use.char); err != nil {
						b.Fatalf("Definition: %v", err)
					}
				}
			})
			b.Run("references", func(b *testing.B) {
				for b.Loop() {
					if _, err := m.References(file, c.refsAt.line, c.refsAt.char); err != nil {
						b.Fatalf("References: %v", err)
					}
				}
			})
		})
	}
}
