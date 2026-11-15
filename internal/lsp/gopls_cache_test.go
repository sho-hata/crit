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

	ws := Workspace{Root: t.TempDir(), Scratch: t.TempDir()}
	cache := goplsCacheDir(ws)
	for rel, f := range files {
		write(t, filepath.Join(cache, rel), f.old)
	}
	// The cache the daemon's own environment names is not the server's.
	other := t.TempDir()
	t.Setenv("GOPLSCACHE", other)
	decoy := filepath.Join(other, "gopls", "h1", "aa", "old")
	write(t, decoy, true)

	sweepGoplsCache(ws)

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
		t.Errorf("file in the daemon's own GOPLSCACHE removed: %v", err)
	}
}

func TestSweepGoplsCacheMissingDirIsNoop(t *testing.T) {
	ws := Workspace{Root: t.TempDir(), Scratch: t.TempDir()}

	sweepGoplsCache(ws)

	if _, err := os.Stat(filepath.Join(goplsCacheDir(ws), "gopls")); !os.IsNotExist(err) {
		t.Errorf("gopls dir was created (stat err = %v)", err)
	}
}

func TestSweepCacheOnceRunsOncePerLanguage(t *testing.T) {
	t.Parallel()

	var sweeps atomic.Int32
	lang := &Language{Name: "go", CacheSweep: func(Workspace) { sweeps.Add(1) }}
	for range 3 {
		lang.sweepCacheOnce(Workspace{})
	}

	waitFor(t, "cache sweep", func() bool { return sweeps.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := sweeps.Load(); got != 1 {
		t.Errorf("sweeps = %d after 3 calls, want 1", got)
	}
	(&Language{Name: "none"}).sweepCacheOnce(Workspace{})
}
