//go:build windows

package session

import (
	"errors"
	"os"
	"path/filepath"
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
		{"other", errors.New("boom"), false},
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
