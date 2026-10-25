package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/sho-hata/crit/internal/flock"
)

const maxWorktreeSlots = 32

// WorktreePool is a repository's range/PR focus worktree slots: <Dir>/<n> is
// slot n's checkout and <Dir>/<n>.lock the file whose OS lock marks the slot
// as in use. The path is fixed per repository so gopls's filecache, keyed by
// absolute path, carries over from one reviewed PR to the next.
type WorktreePool struct{ Dir string }

// WorktreePoolFor returns the pool at ~/.crit/lsp/worktrees/<hash12>, keyed
// by repoRoot.
func WorktreePoolFor(repoRoot string) (WorktreePool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return WorktreePool{}, err
	}
	sum := sha256.Sum256([]byte(repoRoot))
	return WorktreePool{Dir: filepath.Join(home, ".crit", "lsp", "worktrees", hex.EncodeToString(sum[:])[:12])}, nil
}

// WorktreeSlot is a slot this process holds until Release.
type WorktreeSlot struct {
	Dir   string
	Index int
	lock  *os.File
}

// Acquire creates the pool directory and takes the lowest-numbered free slot.
func (p WorktreePool) Acquire() (*WorktreeSlot, error) {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("lsp worktree pool: %w", err)
	}
	for i := range maxWorktreeSlots {
		f, err := p.tryLockSlot(i)
		if err != nil {
			return nil, fmt.Errorf("lsp worktree pool %s: %w", p.Dir, err)
		}
		if f != nil {
			return &WorktreeSlot{Dir: p.slotDir(i), Index: i, lock: f}, nil
		}
	}
	return nil, fmt.Errorf("lsp worktree pool %s: all %d slots are in use", p.Dir, maxWorktreeSlots)
}

// Release frees the slot. Calling it again is a no-op.
func (s *WorktreeSlot) Release() {
	if s.lock == nil {
		return
	}
	_ = flock.Unlock(s.lock)
	s.lock.Close()
	s.lock = nil
}

// Sweep calls remove on each slot directory whose lock is free, holding that
// lock during the call, so checkouts left by crashed daemons don't pile up.
// Slots held by live daemons are skipped.
func (p WorktreePool) Sweep(remove func(dir string) error) error {
	var errs []error
	for i := range maxWorktreeSlots {
		dir := p.slotDir(i)
		if _, err := os.Lstat(dir); err != nil {
			continue
		}
		f, err := p.tryLockSlot(i)
		if err != nil || f == nil {
			continue
		}
		if err := remove(dir); err != nil {
			errs = append(errs, err)
		}
		_ = flock.Unlock(f)
		f.Close()
	}
	return errors.Join(errs...)
}

func (p WorktreePool) tryLockSlot(i int) (*os.File, error) {
	f, err := os.OpenFile(p.slotDir(i)+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	locked, err := flock.TryLock(f)
	if !locked {
		f.Close()
		return nil, err
	}
	return f, nil
}

func (p WorktreePool) slotDir(i int) string {
	return filepath.Join(p.Dir, strconv.Itoa(i))
}
