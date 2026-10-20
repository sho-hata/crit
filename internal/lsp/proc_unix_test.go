//go:build !windows

package lsp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A server's children die with it: typescript-language-server forks
// tsserver, which otherwise outlives Close and keeps writing.
func TestCloseKillsWhatTheServerForked(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child")
	lang := &Language{
		Name:    "forking",
		Command: []string{"sh", "-c", `sleep 60 >/dev/null 2>&1 & echo $! >"$0"`, pidFile},
	}
	_, err := startServer(context.Background(), t.TempDir(), lang, nil, nil)
	if err == nil {
		t.Fatal("startServer succeeded against a server that exits at once")
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d outlived Close", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
