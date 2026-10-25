package lsp

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestSweepGoplsCache(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOPLSCACHE", tmp)
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
	for rel, f := range files {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if f.old {
			if err := os.Chtimes(p, old, old); err != nil {
				t.Fatal(err)
			}
		}
	}

	sweepGoplsCache()

	for rel, f := range files {
		_, err := os.Stat(filepath.Join(tmp, rel))
		if exists := err == nil; exists != f.kept {
			t.Errorf("%s exists = %v, want %v", rel, exists, f.kept)
		}
	}
	if _, err := os.Stat(filepath.Join(tmp, "gopls/h1/aa")); err != nil {
		t.Errorf("directory removed: %v", err)
	}
}

func TestSweepGoplsCacheMissingDirIsNoop(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("GOPLSCACHE", tmp)

	sweepGoplsCache()

	if _, err := os.Stat(filepath.Join(tmp, "gopls")); !os.IsNotExist(err) {
		t.Errorf("gopls dir was created (stat err = %v)", err)
	}
}

func TestSweepCacheOnceRunsOncePerLanguage(t *testing.T) {
	t.Parallel()

	var sweeps atomic.Int32
	lang := &Language{Name: "go", CacheSweep: func() { sweeps.Add(1) }}
	for range 3 {
		lang.sweepCacheOnce()
	}

	waitFor(t, "cache sweep", func() bool { return sweeps.Load() >= 1 })
	time.Sleep(50 * time.Millisecond)
	if got := sweeps.Load(); got != 1 {
		t.Errorf("sweeps = %d after 3 calls, want 1", got)
	}
	(&Language{Name: "none"}).sweepCacheOnce()
}
