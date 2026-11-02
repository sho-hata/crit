package flock

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func openLockFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestTryLockContention(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, second := openLockFile(t, path), openLockFile(t, path)

	if ok, err := TryLock(first); !ok || err != nil {
		t.Fatalf("TryLock(first) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := TryLock(second); ok || err != nil {
		t.Fatalf("TryLock(second) while first holds = %v, %v; want false, nil", ok, err)
	}
	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	if ok, err := TryLock(second); !ok || err != nil {
		t.Fatalf("TryLock(second) after Unlock = %v, %v; want true, nil", ok, err)
	}
}

func TestLockBlocksUntilUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.lock")
	first, second := openLockFile(t, path), openLockFile(t, path)
	if err := Lock(first); err != nil {
		t.Fatal(err)
	}

	acquired := make(chan error, 1)
	go func() { acquired <- Lock(second) }()
	select {
	case err := <-acquired:
		t.Fatalf("Lock(second) returned (%v) while first holds the lock", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := Unlock(first); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-acquired:
		if err != nil {
			t.Fatalf("Lock(second) = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Lock(second) still blocked after Unlock")
	}
}
