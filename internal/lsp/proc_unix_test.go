//go:build !windows

package lsp

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestWaitProcessExitPreservesLeader(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	isolate(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := waitProcessExit(cmd); err != nil {
		t.Fatal(err)
	}
	killTree(cmd)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("exit monitoring reaped the leader before group cleanup: %v", err)
	}
}

// The helper speaks enough LSP to initialize, then either crashes while a
// child holds both pipes, ignores exit, or records a graceful exit.
func TestServerExitHelper(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "lsp-exit-helper" {
		return
	}
	mode, marker := os.Args[len(os.Args)-2], os.Args[len(os.Args)-1]
	r := bufio.NewReader(os.Stdin)
	for {
		msg, err := readFrame(r)
		if err != nil {
			if mode == "ignore" {
				time.Sleep(time.Hour)
			}
			_ = os.WriteFile(marker, []byte("eof"), 0o600)
			os.Exit(0)
		}
		switch msg.Method {
		case "initialize", "shutdown":
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":null}`, *msg.ID)
			fmt.Fprintf(os.Stdout, "Content-Length: %d\r\n\r\n%s", len(body), body)
		case "initialized":
			if mode == "crash" {
				child := exec.Command("sleep", "60")
				child.Stdin, child.Stdout = os.Stdin, os.Stdout
				if err := child.Start(); err != nil {
					os.Exit(2)
				}
				os.Exit(0)
			}
		case "exit":
			if mode == "ignore" {
				time.Sleep(time.Hour)
			}
			if err := os.WriteFile(marker, []byte("graceful"), 0o600); err != nil {
				os.Exit(3)
			}
			os.Exit(0)
		}
	}
}

func TestServerExitLifecycle(t *testing.T) {
	for _, mode := range []string{"crash", "graceful", "ignore"} {
		t.Run(mode, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "exit")
			lang := &Language{
				Name:    "exit-helper",
				Command: []string{os.Args[0], "-test.run=^TestServerExitHelper$", "lsp-exit-helper", mode, marker},
			}
			client, err := startServer(context.Background(), t.TempDir(), lang, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(client.Close)
			if mode == "crash" {
				select {
				case <-client.done:
				case <-time.After(5 * time.Second):
					t.Fatal("crashed leader was not detected while its child held the pipes")
				}
			}
			client.Close()
			if mode == "graceful" {
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("server was killed before its exit handler ran: %v", err)
				}
			}
		})
	}
}

// The daemon's context is cancelled before it shuts the servers down, so
// cancellation must reach the server as stdin EOF, which it exits on.
func TestContextCancelLetsTheServerExit(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "exit")
	lang := &Language{
		Name:    "exit-helper",
		Command: []string{os.Args[0], "-test.run=^TestServerExitHelper$", "lsp-exit-helper", "graceful", marker},
	}
	ctx, cancel := context.WithCancel(context.Background())
	client, err := startServer(ctx, t.TempDir(), lang, nil, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	cancel()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if raw, err := os.ReadFile(marker); err == nil {
			if string(raw) != "eof" {
				t.Fatalf("marker = %q, want the EOF exit path", raw)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("cancellation killed the server before it could exit on its own")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

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
	// A reaper is nobody's job in a container without one, so a killed
	// child may stay a zombie: dead all the same.
	deadline := time.Now().Add(5 * time.Second)
	for !processDead(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("child %d outlived Close", pid)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
