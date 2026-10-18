package lsp

// Opt-in: references from review files nobody hovered, against the real
// servers that need them opened (Language.OpenSessionFile).
//
//	CRIT_LSP_REAL=1 go test ./internal/lsp -run TestRealSessionFiles -v

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRealSessionFilesFindReferences(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		files      map[string]string
		target     string // holds the definition queried
		line, char int
		callers    []string
	}{
		{
			// No tsconfig.json: the project is inferred from the open files.
			name: "typescript",
			files: map[string]string{
				"a.ts": "export function foo() { return 1 }\n",
				"b.ts": "import { foo } from './a'\nfoo()\n",
				"c.ts": "import { foo } from './a'\nexport const x = foo()\n",
			},
			target: "a.ts", line: 0, char: 17,
			callers: []string{"b.ts", "c.ts"},
		},
		{
			// tsconfig.json without allowJs: c.js is in an inferred project
			// of its own, next to the configured one holding a.ts and b.ts.
			name: "javascript beside a tsconfig without allowJs",
			files: map[string]string{
				"tsconfig.json": `{"compilerOptions":{"strict":true}}`,
				"a.ts":          "export function foo() { return 1 }\n",
				"b.ts":          "import { foo } from './a'\nfoo()\n",
				"c.js":          "import { foo } from './a'\nfoo()\n",
			},
			target: "a.ts", line: 0, char: 17,
			callers: []string{"b.ts", "c.js"},
		},
		{
			// Right after start pyright has analyzed only the open files.
			name: "python",
			files: map[string]string{
				"a.py":     "def foo():\n    return 1\n",
				"b.py":     "from a import foo\nfoo()\n",
				"pkg/c.py": "from a import foo\nx = foo()\n",
			},
			target: "a.py", line: 0, char: 4,
			callers: []string{"b.py", "pkg/c.py"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			requireRealLanguage(t, LanguageForPath(tc.target))

			dir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			rels := []string{tc.target}
			for name, body := range tc.files {
				abs := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
				if name != tc.target {
					rels = append(rels, name)
				}
			}
			m := NewManager(dir, "", context.Background())
			t.Cleanup(m.Shutdown)
			m.SetSessionFiles(func() []string { return rels })

			refs, err := m.References(filepath.Join(dir, tc.target), tc.line, tc.char)
			if err != nil {
				t.Fatalf("References: %v", err)
			}
			found := map[string]bool{}
			for _, r := range refs {
				rel, _ := filepath.Rel(dir, r.Path)
				found[filepath.ToSlash(rel)] = true
			}
			for _, caller := range tc.callers {
				if !found[caller] {
					t.Errorf("no reference from %s; got %v", caller, refs)
				}
			}
		})
	}
}
