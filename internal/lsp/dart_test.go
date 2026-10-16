package lsp

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeDartSDK lays out an SDK (lib/core/core.dart) under dir and returns the
// path of its dart binary.
func writeDartSDK(t *testing.T, dir string) string {
	t.Helper()
	for _, p := range []string{"lib/core/core.dart", "bin/dart"} {
		path := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return filepath.Join(dir, "bin", "dart")
}

func TestDartSDKRootFor(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		// setup lays out a tree under base and returns the dart binary and
		// the SDK root expected, "" for none.
		setup func(t *testing.T, base string) (bin, want string)
	}{
		{"plain sdk", func(t *testing.T, base string) (string, string) {
			sdk := filepath.Join(base, "dart-sdk")
			return writeDartSDK(t, sdk), sdk
		}},
		{"flutter wrapper script", func(t *testing.T, base string) (string, string) {
			flutter := filepath.Join(base, "flutter")
			sdk := filepath.Join(flutter, "bin", "cache", "dart-sdk")
			writeDartSDK(t, sdk)
			wrapper := filepath.Join(flutter, "bin", "dart")
			if err := os.WriteFile(wrapper, []byte("#!/bin/sh\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			return wrapper, sdk
		}},
		// A bin/dart with no lib/core next to it is not an SDK we can read.
		{"unknown layout", func(t *testing.T, base string) (string, string) {
			bin := filepath.Join(base, "shims", "dart")
			if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(bin, nil, 0o755); err != nil {
				t.Fatal(err)
			}
			return bin, ""
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bin, want := tc.setup(t, t.TempDir())
			if got := dartSDKRootFor(bin); got != want {
				t.Errorf("dartSDKRootFor(%s) = %q, want %q", bin, got, want)
			}
		})
	}
}

// Not parallel: it sets the process environment.
func TestPubCacheDir(t *testing.T) {
	existing := t.TempDir()
	cases := []struct {
		name string
		env  string
		want string
	}{
		{"PUB_CACHE wins", existing, existing},
		{"PUB_CACHE that does not exist", filepath.Join(existing, "missing"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PUB_CACHE", tc.env)
			if got := pubCacheDir(); got != tc.want {
				t.Errorf("pubCacheDir() = %q, want %q", got, tc.want)
			}
		})
	}

	t.Run("default location", func(t *testing.T) {
		home := t.TempDir()
		t.Setenv("PUB_CACHE", "")
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		want := filepath.Join(home, ".pub-cache")
		if runtime.GOOS == "windows" {
			t.Setenv("LOCALAPPDATA", home)
			want = filepath.Join(home, "Pub", "Cache")
		}
		if got := pubCacheDir(); got != "" {
			t.Errorf("pubCacheDir() = %q before the cache exists, want empty", got)
		}
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		if got := pubCacheDir(); got != want {
			t.Errorf("pubCacheDir() = %q, want %q", got, want)
		}
	})
}
