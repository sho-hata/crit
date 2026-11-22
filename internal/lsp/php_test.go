package lsp

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

// writeComposerVendor returns the vendor dir it creates.
func writeComposerVendor(t *testing.T, dir string) string {
	t.Helper()
	vendor := filepath.Join(dir, "vendor")
	if err := os.MkdirAll(filepath.Join(vendor, "composer"), 0o755); err != nil {
		t.Fatal(err)
	}
	return vendor
}

func TestFindComposerVendor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// setup lays out the tree under base (root is base/repo) and returns
		// the file the request is about and the vendor dir expected, "" for none.
		setup func(t *testing.T, base, root string) (src, want string)
	}{
		{"vendor at root", func(t *testing.T, _, root string) (string, string) {
			return filepath.Join(root, "src", "Http", "App.php"), writeComposerVendor(t, root)
		}},
		{"nearest vendor wins", func(t *testing.T, _, root string) (string, string) {
			writeComposerVendor(t, root)
			pkg := filepath.Join(root, "packages", "api")
			return filepath.Join(pkg, "src", "App.php"), writeComposerVendor(t, pkg)
		}},
		{"file outside the package falls back to the root vendor", func(t *testing.T, _, root string) (string, string) {
			writeComposerVendor(t, filepath.Join(root, "packages", "api"))
			return filepath.Join(root, "bin", "tool.php"), writeComposerVendor(t, root)
		}},
		// A Go or Ruby vendor/ has no composer/ inside and must not become an
		// include path.
		{"vendor that is not Composer's", func(t *testing.T, _, root string) (string, string) {
			if err := os.MkdirAll(filepath.Join(root, "vendor", "github.com"), 0o755); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(root, "app.php"), ""
		}},
		{"no vendor", func(_ *testing.T, _, root string) (string, string) {
			return filepath.Join(root, "app.php"), ""
		}},
		{"file outside root", func(t *testing.T, base, _ string) (string, string) {
			other := filepath.Join(base, "other")
			writeComposerVendor(t, other)
			return filepath.Join(other, "app.php"), ""
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
			if got := findComposerVendor(root, src); got != want {
				t.Errorf("findComposerVendor = %q, want %q", got, want)
			}
		})
	}
}

func TestPHPConfigSettings(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	app := filepath.Join(root, "app.php")

	if got := phpConfigSettings(root, app); got != nil {
		t.Errorf("phpConfigSettings without a vendor dir = %v, want nil", got)
	}

	vendor := writeComposerVendor(t, root)
	want := map[string]any{
		"intelephense": map[string]any{
			"environment": map[string]any{"includePaths": []string{vendor}},
		},
	}
	if got := phpConfigSettings(root, app); !reflect.DeepEqual(got, want) {
		t.Errorf("phpConfigSettings = %v, want %v", got, want)
	}
}

func TestPHPExtraRootsVendor(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	vendor := writeComposerVendor(t, root)
	var got *PeekRoot
	for _, r := range phpExtraRoots(root, noRun) {
		if r.Label == "$VENDOR" {
			got = &r
		}
	}
	if got == nil || got.Path != vendor || !got.LocalEnv {
		t.Errorf("$VENDOR root = %+v, want %s marked LocalEnv", got, vendor)
	}
}

// installIntelephense lays out the npm package under prefix, with <prefix>/bin
// linking to lib/intelephense.js and the stubs beside it in lib/stub. It
// returns the bin and stub dirs.
func installIntelephense(t *testing.T, prefix string) (bin, stub string) {
	t.Helper()
	lib := filepath.Join(prefix, "lib", "node_modules", "intelephense", "lib")
	stub = filepath.Join(lib, "stub")
	if err := os.MkdirAll(stub, 0o755); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(lib, "intelephense.js")
	if err := os.WriteFile(entry, []byte("#!/usr/bin/env node\n"), 0o755); err != nil { //nolint:gosec // LookPath needs it executable
		t.Fatal(err)
	}
	bin = filepath.Join(prefix, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(entry, filepath.Join(bin, "intelephense")); err != nil {
		t.Fatal(err)
	}
	return bin, stub
}

func phpStubsRoot(roots []PeekRoot) string {
	for _, r := range roots {
		if r.Label == "$PHP_STUBS" {
			return r.Path
		}
	}
	return ""
}

// A peek root is readable through the API, so a copy of intelephense in the
// tree under review, ahead on PATH, must not name it: its stub dir can be a
// symlink to anywhere. The sandbox never runs that copy either.
func TestPHPExtraRootsSkipAServerInTheTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	repo := realTempDir(t)
	localBin, localStub := installIntelephense(t, filepath.Join(repo, "node_modules", ".prefix"))
	if err := os.Remove(localStub); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realTempDir(t), localStub); err != nil {
		t.Fatal(err)
	}
	globalBin, globalStub := installIntelephense(t, realTempDir(t))
	sep := string(os.PathListSeparator)

	t.Setenv("PATH", localBin+sep+globalBin)
	if got := phpStubsRoot(phpExtraRoots(repo, noRun)); got != globalStub {
		t.Errorf("$PHP_STUBS = %q, want the installed %s", got, globalStub)
	}

	t.Setenv("PATH", localBin)
	if got := phpStubsRoot(phpExtraRoots(repo, noRun)); got != "" {
		t.Errorf("$PHP_STUBS with only the tree's copy on PATH = %q, want none", got)
	}
}

func TestPHPInitOptionsKeepTheIndexInTheScratch(t *testing.T) {
	t.Parallel()
	lang := LanguageForPath("index.php")
	if lang == nil || lang.InitOptions == nil {
		t.Fatal("the php language registers no InitOptions")
	}
	ws := Workspace{Root: t.TempDir(), Scratch: t.TempDir()}
	opts := lang.InitOptions(ws, filepath.Join(ws.Root, "index.php"))
	tmp := filepath.Join(ws.Scratch, "tmp")
	for _, key := range []string{"storagePath", "globalStoragePath"} {
		dir, _ := opts[key].(string)
		rel, err := filepath.Rel(ws.Scratch, dir)
		if dir == "" || err != nil || !filepath.IsLocal(rel) {
			t.Errorf("%s = %q, want a directory under the scratch %s", key, dir, ws.Scratch)
			continue
		}
		if dir == tmp || strings.HasPrefix(dir, tmp+string(filepath.Separator)) {
			t.Errorf("%s = %q lies under tmp/, which every start empties", key, dir)
		}
	}
	if a, b := opts["storagePath"], opts["globalStoragePath"]; a == b {
		t.Errorf("storagePath and globalStoragePath are both %v", a)
	}
}
