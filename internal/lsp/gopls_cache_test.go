package lsp

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSweepGoplsCache(t *testing.T) {
	old := time.Now().Add(-goplsCacheMaxAge - time.Hour)
	files := map[string]struct {
		old  bool
		kept bool
	}{
		"gopls/h1/aa/old":  {old: true, kept: false},
		"gopls/h1/aa/new":  {old: false, kept: true},
		"outside-gopls":    {old: true, kept: true},
		"gopls/h2/bb/old2": {old: true, kept: false},
	}
	write := func(t *testing.T, rel string, isOld bool) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(rel), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(rel, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if isOld {
			if err := os.Chtimes(rel, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	tests := []struct {
		name string
		// env returns GOPLSCACHE and the directory it names, given the
		// server root.
		env func(root string) (value, cache string)
	}{
		{"absolute", func(string) (string, string) {
			dir := t.TempDir()
			return dir, dir
		}},
		{"relative to server root", func(root string) (string, string) {
			return ".cache", filepath.Join(root, ".cache")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			value, cache := tt.env(root)
			t.Setenv("GOPLSCACHE", value)
			for rel, f := range files {
				write(t, filepath.Join(cache, rel), f.old)
			}
			// The daemon's cwd differs from the server root under range
			// focus; a relative GOPLSCACHE must not resolve against it.
			cwd := t.TempDir()
			decoy := filepath.Join(cwd, ".cache", "gopls", "h1", "aa", "old")
			write(t, decoy, true)
			t.Chdir(cwd)

			sweepGoplsCache(root)

			for rel, f := range files {
				_, err := os.Stat(filepath.Join(cache, rel))
				if exists := err == nil; exists != f.kept {
					t.Errorf("%s exists = %v, want %v", rel, exists, f.kept)
				}
			}
			if _, err := os.Stat(filepath.Join(cache, "gopls/h1/aa")); err != nil {
				t.Errorf("directory removed: %v", err)
			}
			if _, err := os.Stat(decoy); err != nil {
				t.Errorf("file under the daemon's cwd removed: %v", err)
			}
		})
	}
}

func TestSweepGoplsCacheMissingDirIsNoop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOPLSCACHE", tmp)

	sweepGoplsCache(t.TempDir())

	if _, err := os.Stat(filepath.Join(tmp, "gopls")); !os.IsNotExist(err) {
		t.Errorf("gopls dir was created (stat err = %v)", err)
	}
}

func TestSweepCacheOnceRunsOncePerLanguage(t *testing.T) {
	t.Parallel()

	var sweeps atomic.Int32
	lang := &Language{Name: "go", CacheSweep: func(string) { sweeps.Add(1) }}
	for range 3 {
		lang.sweepCacheOnce("")
	}

	waitFor(t, "cache sweep", func() bool { return sweeps.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := sweeps.Load(); got != 1 {
		t.Errorf("sweeps = %d after 3 calls, want 1", got)
	}
	(&Language{Name: "none"}).sweepCacheOnce("")
}
