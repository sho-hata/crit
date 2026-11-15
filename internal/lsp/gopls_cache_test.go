package lsp

import (
	"os"
	"path/filepath"
	"runtime"
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

// The server can write its cache directory, so it can leave a symlink
// there. The sweep runs in the daemon, unconfined: it must not follow one
// out of the scratch and delete somebody else's old files.
func TestSweepGoplsCacheStaysInsideTheScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("needs symlinks")
	}
	old := time.Now().Add(-goplsCacheMaxAge - time.Hour)
	outside := t.TempDir()
	victim := filepath.Join(outside, "gopls", "h1", "aa", "old")
	if err := os.MkdirAll(filepath.Dir(victim), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victim, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(victim, old, old); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"goplscache", filepath.Join("goplscache", "gopls")} {
		ws := Workspace{Root: t.TempDir(), Scratch: t.TempDir()}
		at := filepath.Join(ws.Scratch, link)
		target := outside
		if filepath.Base(link) == "gopls" {
			target = filepath.Join(outside, "gopls")
		}
		if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, at); err != nil {
			t.Fatal(err)
		}

		sweepGoplsCache(ws)

		if _, err := os.Stat(victim); err != nil {
			t.Fatalf("symlink at %s: the sweep deleted a file outside the scratch: %v", link, err)
		}
	}
}

// The cache is per scratch, so each scratch is swept once, not each language.
func TestSweepCacheOnceRunsOncePerScratch(t *testing.T) {
	t.Parallel()

	var sweeps atomic.Int32
	lang := &Language{Name: "go", CacheSweep: func(Workspace) { sweeps.Add(1) }}
	first, second := Workspace{Scratch: t.TempDir()}, Workspace{Scratch: t.TempDir()}
	for range 3 {
		lang.sweepCacheOnce(first)
	}
	lang.sweepCacheOnce(second)

	waitFor(t, "cache sweeps", func() bool { return sweeps.Load() >= 2 })
	time.Sleep(50 * time.Millisecond)
	if got := sweeps.Load(); got != 2 {
		t.Errorf("sweeps = %d over two scratches, want 2", got)
	}
	(&Language{Name: "none"}).sweepCacheOnce(first)
}
