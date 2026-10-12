package lsp

import (
	"os"
	"path/filepath"
	"reflect"
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
	for _, r := range phpExtraRoots(root) {
		if r.Label == "$VENDOR" {
			got = &r
		}
	}
	if got == nil || got.Path != vendor || !got.LocalEnv {
		t.Errorf("$VENDOR root = %+v, want %s marked LocalEnv", got, vendor)
	}
}
