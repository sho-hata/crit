package lsp

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/sho-hata/crit/internal/testutil"
)

func acquireSlot(t *testing.T, p WorktreePool) *WorktreeSlot {
	t.Helper()
	s, err := p.Acquire(func(string) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Release)
	return s
}

func TestWorktreePoolAcquireTakesLowestFreeSlot(t *testing.T) {
	p := WorktreePool{Dir: filepath.Join(t.TempDir(), "pool")}

	first := acquireSlot(t, p)
	second := acquireSlot(t, p)
	if first.Index != 0 || second.Index != 1 {
		t.Fatalf("indexes = %d, %d; want 0, 1", first.Index, second.Index)
	}
	if want := filepath.Join(p.Dir, "1"); second.Dir != want {
		t.Errorf("second.Dir = %q, want %q", second.Dir, want)
	}

	first.Release()
	first.Release()
	if again := acquireSlot(t, p); again.Index != 0 {
		t.Errorf("Acquire after Release(0) = %d, want 0", again.Index)
	}
	if third := acquireSlot(t, p); third.Index != 2 {
		t.Errorf("Acquire while 0 and 1 are held = %d, want 2", third.Index)
	}
}

func TestWorktreePoolAcquireFailsWithoutPoolDir(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(parent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := (WorktreePool{Dir: filepath.Join(parent, "pool")}).Acquire(os.RemoveAll); err == nil {
		s.Release()
		t.Fatal("Acquire under a regular file succeeded")
	}
}

func TestWorktreePoolAcquireRemovesLeftover(t *testing.T) {
	p := WorktreePool{Dir: t.TempDir()}
	leftover := filepath.Join(p.Dir, "0")
	if err := os.Mkdir(leftover, 0o700); err != nil {
		t.Fatal(err)
	}

	var removed []string
	s, err := p.Acquire(func(dir string) error {
		removed = append(removed, dir)
		return os.RemoveAll(dir)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Release)
	if s.Index != 0 || !slices.Equal(removed, []string{leftover}) {
		t.Errorf("slot %d, removed %v; want slot 0 with %s removed", s.Index, removed, leftover)
	}
}

func TestWorktreePoolAcquireSkipsUnremovableSlot(t *testing.T) {
	p := WorktreePool{Dir: t.TempDir()}
	stuck := filepath.Join(p.Dir, "0")
	if err := os.Mkdir(stuck, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		remove func(string) error
	}{
		{name: "remove fails", remove: func(string) error { return errors.New("boom") }},
		{name: "remove leaves the directory", remove: func(string) error { return nil }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, err := p.Acquire(tt.remove)
			if err != nil {
				t.Fatal(err)
			}
			s.Release()
			if s.Index != 1 {
				t.Errorf("Acquire = slot %d, want 1 (slot 0 still holds a leftover)", s.Index)
			}
		})
	}

	if _, err := os.Stat(stuck); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < maxWorktreeSlots; i++ {
		if err := os.Mkdir(p.slotDir(i), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	boom := errors.New("boom")
	if s, err := p.Acquire(func(string) error { return boom }); !errors.Is(err, boom) {
		if s != nil {
			s.Release()
		}
		t.Errorf("Acquire with every slot unremovable = %v, want an error wrapping %v", err, boom)
	}
}

func TestWorktreePoolSweep(t *testing.T) {
	removeErr := errors.New("boom")
	tests := []struct {
		name    string
		remove  error
		wantErr error
	}{
		{name: "removes free slots", remove: nil, wantErr: nil},
		{name: "surfaces remove errors", remove: removeErr, wantErr: removeErr},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := WorktreePool{Dir: t.TempDir()}
			held := acquireSlot(t, p)
			for _, dir := range []string{held.Dir, filepath.Join(p.Dir, "1")} {
				if err := os.Mkdir(dir, 0o700); err != nil {
					t.Fatal(err)
				}
			}

			var removed []string
			err := p.Sweep(func(dir string) error {
				removed = append(removed, dir)
				return tt.remove
			})
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Sweep error = %v, want %v", err, tt.wantErr)
			}
			if want := []string{filepath.Join(p.Dir, "1")}; !slices.Equal(removed, want) {
				t.Errorf("removed %v, want %v", removed, want)
			}
		})
	}
}

func TestWorktreePoolFor(t *testing.T) {
	home := t.TempDir()
	testutil.SetHome(t, home)

	a1, err := WorktreePoolFor("/repo/a")
	if err != nil {
		t.Fatal(err)
	}
	a2, _ := WorktreePoolFor("/repo/a")
	b, _ := WorktreePoolFor("/repo/b")
	if a1 != a2 {
		t.Errorf("same repo gave %q and %q", a1.Dir, a2.Dir)
	}
	if a1 == b {
		t.Errorf("different repos share %q", a1.Dir)
	}
	if got, want := filepath.Dir(a1.Dir), filepath.Join(home, ".crit", "lsp", "worktrees"); got != want {
		t.Errorf("pool parent = %q, want %q", got, want)
	}
}
