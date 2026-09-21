package lsp

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// writeVenv makes dir/name look like a virtualenv — its site-packages exists —
// and returns that site-packages path.
func writeVenv(t *testing.T, dir, name string) string {
	t.Helper()
	sp := filepath.Join(dir, name, "lib", "python3.13", "site-packages")
	if err := os.MkdirAll(sp, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", sp, err)
	}
	return sp
}

func TestFindSitePackages(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// setup lays out the tree under base (root is base/repo) and returns
		// the file the request is about and the site-packages expected, "" for none.
		setup func(t *testing.T, base, root string) (src, want string)
	}{
		{"venv at root", func(t *testing.T, _, root string) (string, string) {
			return filepath.Join(root, "src", "deep", "app.py"), writeVenv(t, root, ".venv")
		}},
		{"venv without the dot", func(t *testing.T, _, root string) (string, string) {
			return filepath.Join(root, "app.py"), writeVenv(t, root, "venv")
		}},
		{".venv is preferred over venv", func(t *testing.T, _, root string) (string, string) {
			writeVenv(t, root, "venv")
			return filepath.Join(root, "app.py"), writeVenv(t, root, ".venv")
		}},
		// A monorepo keeps one venv per service; the one nearest the file is
		// the one its imports resolve against.
		{"nearest venv wins", func(t *testing.T, _, root string) (string, string) {
			writeVenv(t, root, ".venv")
			svc := filepath.Join(root, "services", "api")
			return filepath.Join(svc, "app", "main.py"), writeVenv(t, svc, ".venv")
		}},
		{"file outside the service falls back to the root venv", func(t *testing.T, _, root string) (string, string) {
			writeVenv(t, filepath.Join(root, "services", "api"), ".venv")
			return filepath.Join(root, "tools", "gen.py"), writeVenv(t, root, ".venv")
		}},
		{"windows layout", func(t *testing.T, _, root string) (string, string) {
			sp := filepath.Join(root, ".venv", "Lib", "site-packages")
			if err := os.MkdirAll(sp, 0o755); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "app.py"), sp
		}},
		// Only a directory that really is a venv counts: an unrelated venv/
		// folder must not become a search path.
		{"directory that is not a venv", func(t *testing.T, _, root string) (string, string) {
			if err := os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "app.py"), ""
		}},
		{"no venv", func(_ *testing.T, _, root string) (string, string) {
			return filepath.Join(root, "app.py"), ""
		}},
		// The walk stops at root: a file outside the workspace is not ours to
		// resolve dependencies for.
		{"file outside root", func(t *testing.T, base, _ string) (string, string) {
			other := filepath.Join(base, "other")
			writeVenv(t, other, ".venv")
			return filepath.Join(other, "app.py"), ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			base := t.TempDir()
			root := filepath.Join(base, "repo")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			src, want := tc.setup(t, base, root)
			if got := findSitePackages(root, src); got != want {
				t.Errorf("findSitePackages = %q, want %q", got, want)
			}
		})
	}
}

func TestPyConfigSettings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	app := filepath.Join(root, "app.py")

	if got := pyConfigSettings(root, app); got != nil {
		t.Errorf("pyConfigSettings without a venv = %v, want nil (pyright resolves on its own)", got)
	}

	sp := writeVenv(t, root, ".venv")
	want := map[string]any{
		"python": map[string]any{
			"analysis": map[string]any{"extraPaths": []string{sp}},
		},
	}
	if got := pyConfigSettings(root, app); !reflect.DeepEqual(got, want) {
		t.Errorf("pyConfigSettings = %v, want %v", got, want)
	}
}

// pyright must never be handed an interpreter to run: the venv would come from
// the tree under review. Pin the shape so pythonPath cannot creep back in.
func TestPyConfigSettingsNeverNamesAnInterpreter(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	writeVenv(t, root, ".venv")
	got := pyConfigSettings(root, filepath.Join(root, "app.py"))
	py, _ := got["python"].(map[string]any)
	for _, key := range []string{"pythonPath", "defaultInterpreterPath", "venvPath"} {
		if _, ok := py[key]; ok {
			t.Errorf("python.%s is set; pyright would execute a repo-supplied interpreter", key)
		}
	}
}

func TestParsePythonEnv(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		out  string
		want []PeekRoot
	}{
		{"stdlib and site-packages", "/py/lib/python3.13\n/py/lib/python3.13/site-packages\n", []PeekRoot{
			{Path: "/py/lib/python3.13", Label: "$PYTHON_STDLIB"},
			{Path: "/py/lib/python3.13/site-packages", Label: "$SITE_PACKAGES"},
		}},
		{"no trailing newline", "/py/lib\n/py/site", []PeekRoot{
			{Path: "/py/lib", Label: "$PYTHON_STDLIB"},
			{Path: "/py/site", Label: "$SITE_PACKAGES"},
		}},
		{"windows line endings", "C:\\py\\Lib\r\nC:\\py\\Lib\\site-packages\r\n", []PeekRoot{
			{Path: "C:\\py\\Lib", Label: "$PYTHON_STDLIB"},
			{Path: "C:\\py\\Lib\\site-packages", Label: "$SITE_PACKAGES"},
		}},
		{"stdlib only", "/py/lib\n", []PeekRoot{{Path: "/py/lib", Label: "$PYTHON_STDLIB"}}},
		{"empty output", "", nil},
		{"whitespace only", " \n\n", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := parsePythonEnv(tc.out); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("parsePythonEnv(%q) = %v, want %v", tc.out, got, tc.want)
			}
		})
	}
}

// The venv named by the root the Manager passes in is a peek root: under a
// range/PR focus that root is the working tree, outside the LSP worktree.
func TestPyExtraRootsIncludesTheVenvOfRoot(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	sp := writeVenv(t, root, ".venv")
	found := false
	for _, r := range pyExtraRoots(root) {
		if r.Path == sp {
			found = true
			if r.Label != "$SITE_PACKAGES" {
				t.Errorf("venv label = %q, want $SITE_PACKAGES", r.Label)
			}
		}
	}
	if !found {
		t.Errorf("pyExtraRoots(%q) has no entry for the venv %q", root, sp)
	}
}

// Go and TypeScript keep the wire exactly as it was: neither takes
// workspace/configuration, and TypeScript still waits for startup progress
// because it answers from a half-built project.
func TestRegisteredLanguageHooks(t *testing.T) {
	t.Parallel()

	cases := []struct {
		path           string
		lang           string
		configSettings bool
		skipReadyWait  bool
	}{
		{"main.go", "go", false, false},
		{"App.tsx", "typescript", false, false},
		{"app.py", "python", true, true},
		{"stubs.pyi", "python", true, true},
	}
	for _, tc := range cases {
		lang := LanguageForPath(tc.path)
		if lang == nil || lang.Name != tc.lang {
			t.Fatalf("LanguageForPath(%q) = %v, want %s", tc.path, lang, tc.lang)
		}
		if got := lang.ConfigSettings != nil; got != tc.configSettings {
			t.Errorf("%s: ConfigSettings set = %v, want %v", lang.Name, got, tc.configSettings)
		}
		if lang.SkipReadyWait != tc.skipReadyWait {
			t.Errorf("%s: SkipReadyWait = %v, want %v", lang.Name, lang.SkipReadyWait, tc.skipReadyWait)
		}
	}
}
