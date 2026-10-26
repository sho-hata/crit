package lsp

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
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

// Acquire creates the pool directory and takes the lowest-numbered free slot,
// calling remove on a checkout left in it (a crashed daemon, a failed
// removal) while holding its lock. A slot whose leftover remove fails on is
// skipped: the checkout refuses an existing directory, so handing it out
// would break range/PR focus LSP for the whole repository.
func (p WorktreePool) Acquire(remove func(dir string) error) (*WorktreeSlot, error) {
	if err := os.MkdirAll(p.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("lsp worktree pool: %w", err)
	}
	var errs []error
	for i := range maxWorktreeSlots {
		f, err := p.tryLockSlot(i)
		if err != nil {
			return nil, fmt.Errorf("lsp worktree pool %s: %w", p.Dir, err)
		}
		if f == nil {
			continue
		}
		dir := p.slotDir(i)
		if err := removeLeftover(dir, remove); err != nil {
			unlockSlot(f)
			errs = append(errs, err)
			continue
		}
		return &WorktreeSlot{Dir: dir, Index: i, lock: f}, nil
	}
	return nil, fmt.Errorf("lsp worktree pool %s: no free slot among %d: %w", p.Dir, maxWorktreeSlots, errors.Join(errs...))
}

func removeLeftover(dir string, remove func(dir string) error) error {
	if _, err := os.Lstat(dir); errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err := remove(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(dir); !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("%s still exists after removal", dir)
	}
	return nil
}

// Release frees the slot. Calling it again is a no-op.
func (s *WorktreeSlot) Release() {
	if s.lock == nil {
		return
	}
	unlockSlot(s.lock)
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
		unlockSlot(f)
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

func unlockSlot(f *os.File) {
	_ = flock.Unlock(f)
	f.Close()
}

func (p WorktreePool) slotDir(i int) string {
	return filepath.Join(p.Dir, strconv.Itoa(i))
}
