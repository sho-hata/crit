package lsp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// managerHarness tracks the fake servers a Manager spawned via its start hook.
type managerHarness struct {
	mu      sync.Mutex
	servers []*fakeServer
	spawns  atomic.Int32
	handler func(method string, params json.RawMessage) any
}

func newManagerHarness(t *testing.T, handler func(method string, params json.RawMessage) any) (*Manager, *managerHarness) {
	t.Helper()
	h := &managerHarness{handler: handler}
	m := NewManager(t.TempDir(), context.Background())
	m.start = func(_ context.Context, _ string) (*Client, error) {
		h.spawns.Add(1)
		fs := startFake(h.handler)
		h.mu.Lock()
		h.servers = append(h.servers, fs)
		h.mu.Unlock()
		return fs.client, nil
	}
	t.Cleanup(m.Shutdown)
	return m, h
}

func (h *managerHarness) server(i int) *fakeServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.servers[i]
}

func hoverOK(method string, _ json.RawMessage) any {
	if method == "textDocument/hover" {
		return map[string]any{"contents": map[string]any{"kind": "markdown", "value": "doc"}}
	}
	return nil
}

func writeGoFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestManagerLazyStartAndFileSync(t *testing.T) {
	m, h := newManagerHarness(t, hoverOK)
	if h.spawns.Load() != 0 {
		t.Fatal("manager must not spawn before first request")
	}

	file := writeGoFile(t, m.root, "main.go", "package main\n")
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	if h.spawns.Load() != 1 {
		t.Fatalf("spawns = %d, want 1", h.spawns.Load())
	}

	// Unchanged file: second hover must not re-open or re-send content.
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover(2): %v", err)
	}
	// Changed file: expect a didChange.
	if err := os.WriteFile(file, []byte("package main\n\nfunc F() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover(3): %v", err)
	}

	opens, changes := 0, 0
	for _, method := range h.server(0).notificationMethods() {
		switch method {
		case "textDocument/didOpen":
			opens++
		case "textDocument/didChange":
			changes++
		}
	}
	if opens != 1 || changes != 1 {
		t.Errorf("didOpen = %d, didChange = %d; want 1 and 1", opens, changes)
	}
	if h.spawns.Load() != 1 {
		t.Errorf("spawns = %d after three hovers, want 1", h.spawns.Load())
	}
}

func TestManagerIdleShutdownAndRespawn(t *testing.T) {
	m, h := newManagerHarness(t, hoverOK)
	m.idleTimeout = 30 * time.Millisecond
	file := writeGoFile(t, m.root, "main.go", "package main\n")

	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover: %v", err)
	}
	waitFor(t, "idle shutdown", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.client == nil
	})
	// Next request respawns transparently.
	if _, err := m.Hover(file, 0, 0); err != nil {
		t.Fatalf("Hover after idle: %v", err)
	}
	if h.spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2 (respawn after idle)", h.spawns.Load())
	}
}

func TestManagerRestartsAfterCrash(t *testing.T) {
	var crashed atomic.Bool
	var h *managerHarness
	handler := func(method string, params json.RawMessage) any {
		if method == "textDocument/hover" && crashed.CompareAndSwap(false, true) {
			// First hover: simulate a gopls crash mid-request.
			h.server(0).kill()
			return nil
		}
		return hoverOK(method, params)
	}
	var m *Manager
	m, h = newManagerHarness(t, handler)
	file := writeGoFile(t, m.root, "main.go", "package main\n")

	got, err := m.Hover(file, 0, 0)
	if err != nil {
		t.Fatalf("Hover should succeed after transparent restart, got: %v", err)
	}
	if got != "doc" {
		t.Errorf("Hover = %q, want %q", got, "doc")
	}
	if h.spawns.Load() != 2 {
		t.Errorf("spawns = %d, want 2 (original + restart)", h.spawns.Load())
	}
}

func TestManagerMissingFile(t *testing.T) {
	m, _ := newManagerHarness(t, hoverOK)
	if _, err := m.Hover(filepath.Join(m.root, "nope.go"), 0, 0); err == nil {
		t.Error("Hover on missing file should error")
	}
}

func TestGoplsAvailableDoesNotPanic(t *testing.T) {
	_ = GoplsAvailable() // smoke: PATH lookup must be side-effect free
}
