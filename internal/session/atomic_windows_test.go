//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestIsWindowsTransientIOErr(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"not exist", os.ErrNotExist, true},
		{"file not found", windows.Errno(windows.ERROR_FILE_NOT_FOUND), true},
		{"sharing violation", windows.Errno(windows.ERROR_SHARING_VIOLATION), true},
		{"lock violation", windows.Errno(windows.ERROR_LOCK_VIOLATION), true},
		{"access denied", windows.Errno(windows.ERROR_ACCESS_DENIED), true},
		{"syscall sharing violation", syscall.Errno(windows.ERROR_SHARING_VIOLATION), true},
		{"syscall lock violation", syscall.Errno(windows.ERROR_LOCK_VIOLATION), true},
		{"syscall access denied", syscall.Errno(windows.ERROR_ACCESS_DENIED), true},
		{"syscall file not found", syscall.Errno(windows.ERROR_FILE_NOT_FOUND), true},
		{"syscall path not found", syscall.Errno(windows.ERROR_PATH_NOT_FOUND), true},
		{"path error syscall sharing violation", &os.PathError{Op: "open", Path: "review.json", Err: syscall.Errno(windows.ERROR_SHARING_VIOLATION)}, true},
		{"link error syscall access denied", &os.LinkError{Op: "rename", Old: "a", New: "b", Err: syscall.Errno(windows.ERROR_ACCESS_DENIED)}, true},
		{"wrapped path error syscall sharing violation", fmt.Errorf("worker 0 read: %w", &os.PathError{Op: "open", Path: "review.json", Err: syscall.Errno(windows.ERROR_SHARING_VIOLATION)}), true},
		{"wrapped path error path not found", fmt.Errorf("read: %w", &os.PathError{Op: "open", Path: "review.json", Err: syscall.Errno(windows.ERROR_PATH_NOT_FOUND)}), true},
		{"other", errors.New("boom"), false},
		{"other syscall", syscall.Errno(syscall.EINVAL), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := isWindowsTransientIOErr(tc.err); got != tc.want {
				t.Fatalf("isWindowsTransientIOErr(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// A review that was never saved is the common case, not a rename race: it
// must come back as not-exist after a few ms, not the full ~320ms backoff.
func TestReadFileSharedMissingFileReturnsQuickly(t *testing.T) {
	t.Parallel()

	start := time.Now()
	_, err := ReadFileShared(filepath.Join(t.TempDir(), "review.json"))
	elapsed := time.Since(start)
	if !os.IsNotExist(err) {
		t.Fatalf("err = %v, want not-exist", err)
	}
	// 1+2+4ms of sleeps; the bound leaves room for Windows timer granularity.
	if elapsed > 150*time.Millisecond {
		t.Errorf("took %s, want the short not-exist budget", elapsed)
	}
}
